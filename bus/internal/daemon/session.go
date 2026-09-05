package daemon

import (
	"encoding/json"
	"log"
	"time"

	"github.com/antst/agent-sessions/bus/internal/conn"
	"github.com/antst/agent-sessions/bus/internal/protocol"
	"github.com/antst/agent-sessions/bus/internal/structuredprocess"
)

const (
	maxPendingCalls     = 256
	supersedeWriteBound = time.Second
)

type answer struct {
	value any
	code  int
	data  any
}

type routedRequest struct {
	destination *entry
	method      string
	params      any
	reply       chan answer
}

type pendingCall struct {
	request routedRequest
}

type replyEvent struct {
	requestID int64
	leg       int
	answer    answer
}

type supersedeEvent struct{}

type session struct {
	daemon        *Daemon
	wire          *conn.Conn
	inbox         chan any
	identity      *entry
	lastIn        int64
	nextOut       int64
	pending       map[int64]pendingCall
	requests      map[int64]*requestState
	owned         int
	stopping      bool
	closed        bool
	logged        bool
	child         *structuredprocess.Process
	launch        *launch
	committed     bool
	openID        int64
	closeCall     *routedRequest
	closeAnswer   answer
	closeTimer    *time.Timer
	forget        bool
	launchResult  *answer
	processExited bool
}

func newSession(daemon *Daemon) *session {
	return &session{
		daemon:   daemon,
		inbox:    make(chan any, conn.OutboxSize),
		pending:  map[int64]pendingCall{},
		requests: map[int64]*requestState{},
	}
}

func (s *session) run() {
	shutdown := s.daemon.shutdown
	for {
		if s.closed && s.owned == 0 && (s.launch == nil || s.processExited) && s.drain() {
			if s.launch != nil {
				s.finishLane()
			}
			return
		}
		var closeTimer <-chan time.Time
		if s.closeTimer != nil {
			closeTimer = s.closeTimer.C
		}
		select {
		case event := <-s.inbox:
			s.handleEvent(event)
		case <-closeTimer:
			s.closeTimer = nil
			s.hardStop()
		case <-shutdown:
			shutdown = nil
			if s.launch != nil && !s.committed {
				s.abortLaunch(answer{code: protocol.Internal})
			}
			s.orderlyStop()
		}
	}
}

func (s *session) handleEvent(event any) {
	switch value := event.(type) {
	case conn.Frame:
		if !s.stopping {
			s.handleFrame(value)
		}
	case conn.Closed:
		s.connectionClosed()
		if s.launch != nil {
			if !s.committed && s.launchResult == nil {
				s.abortLaunch(answer{code: protocol.SpawnFailed, data: failure(s.child, "worker exited before open")})
			}
			s.orderlyStop()
		}
	case routedRequest:
		if s.stopping || value.destination != s.identity || s.launch != nil && !s.committed {
			value.reply <- answer{code: protocol.NotConnected}
		} else if value.method == "session.close" && s.launch != nil {
			s.beginClose(value)
		} else {
			s.issue(value)
		}
	case replyEvent:
		s.consumeReply(value)
	case supersedeEvent:
		s.supersede()
	case processEvent:
		s.handleProcess(value)
	case spawnTimeout:
		if value.launch == s.launch && !s.committed {
			s.abortLaunch(answer{code: protocol.Timeout})
			s.hardStop()
		}
	}
}

func (s *session) handleFrame(event conn.Frame) {
	frame := event.Value
	if event.Err != nil {
		code := protocol.InvalidFrame
		if s.identity == nil {
			code = protocol.InvalidHello
		}
		s.reject(frame, code)
		return
	}
	if s.identity == nil {
		s.firstHello(frame)
		return
	}
	if !frame.Request {
		if s.launch != nil && frame.ID == s.openID {
			s.finishOpen(frame)
			return
		}
		s.receiveResponse(frame)
		return
	}
	if frame.ID <= s.lastIn {
		s.reject(frame, protocol.InvalidFrame)
		return
	}
	s.lastIn = frame.ID
	if frame.Method == "session.hello" && s.launch != nil {
		s.reject(frame, protocol.InvalidHello)
		return
	}
	if s.launch != nil && !s.committed {
		s.error(frame, protocol.NotCommitted, nil)
		return
	}
	params, err := protocol.DecodeParams(frame.Method, frame.Params)
	if err != nil || !protocol.Allows(frame.Method, true) {
		code := protocol.InvalidFrame
		if frame.Method == "session.hello" {
			code = protocol.InvalidHello
		}
		s.reject(frame, code)
		return
	}
	if frame.Method == "session.hello" {
		hello, ok := params.(*protocol.PeerHello)
		if !ok {
			s.reject(frame, protocol.InvalidHello)
			return
		}
		s.peerHello(frame, hello)
		return
	}
	s.handleRequest(frame, params)
}

func (s *session) firstHello(frame protocol.Frame) {
	if !frame.Request || frame.ID <= s.lastIn || frame.Method != "session.hello" {
		s.reject(frame, protocol.InvalidHello)
		return
	}
	s.lastIn = frame.ID
	params, err := protocol.DecodeParams(frame.Method, frame.Params)
	if err != nil {
		s.reject(frame, protocol.InvalidHello)
		return
	}
	switch hello := params.(type) {
	case *protocol.PeerHello:
		s.peerHello(frame, hello)
	case *protocol.WorkerHello:
		start, ok := s.daemon.directory.claimWorker(s, hello.LaunchToken, hello.Product)
		if !ok {
			s.reject(frame, protocol.InvalidHello)
			return
		}
		start.claimed <- struct{}{}
		s.startLane(start, frame, hello.HelloDescription)
	default:
		s.reject(frame, protocol.InvalidHello)
	}
}

func (s *session) issue(request routedRequest) {
	if len(s.pending) >= maxPendingCalls {
		request.reply <- answer{code: protocol.Busy}
		return
	}
	s.nextOut++
	body, err := protocol.RequestBytes(s.nextOut, request.method, request.params)
	if err != nil || !s.wire.Send(body) {
		request.reply <- answer{code: protocol.NotConnected}
		return
	}
	s.pending[s.nextOut] = pendingCall{request: request}
}

func (s *session) receiveResponse(frame protocol.Frame) {
	call, ok := s.pending[frame.ID]
	if !ok {
		if !s.logged {
			log.Printf("agentbus: dropping unmatched response id %d", frame.ID)
			s.logged = true
		}
		return
	}
	delete(s.pending, frame.ID)
	if call.request.method == "turn.run" {
		s.daemon.directory.finishRun(s.identity, s)
	}
	if call.request.method == "session.close" {
		s.finishCloseResponse(frame)
		return
	}
	if frame.Error != nil {
		var data any
		if len(frame.Error.Data) != 0 {
			_ = json.Unmarshal(frame.Error.Data, &data)
		}
		call.request.reply <- answer{code: frame.Error.Code, data: data}
		return
	}
	value, err := protocol.DecodeResult(call.request.method, frame.Result)
	if err != nil {
		s.wire.Close()
		call.request.reply <- answer{code: protocol.NotConnected}
		return
	}
	call.request.reply <- answer{value: value}
}

func (s *session) connectionClosed() {
	if s.closed {
		return
	}
	s.stopping, s.closed = true, true
	s.wire.OwnerClosed()
	s.daemon.directory.detach(s.identity, s)
	for id, call := range s.pending {
		delete(s.pending, id)
		if call.request.method == "turn.run" {
			s.daemon.directory.finishRun(s.identity, s)
		}
		if s.closeCall != nil && call.request.reply == s.closeCall.reply {
			continue
		}
		call.request.reply <- answer{code: protocol.NotConnected}
	}
	for id := range s.requests {
		delete(s.requests, id)
	}
}

func (s *session) drain() bool {
	for {
		select {
		case event := <-s.inbox:
			s.handleEvent(event)
		default:
			return s.owned == 0
		}
	}
}

func (s *session) reject(frame protocol.Frame, code int) {
	s.stopping = true
	if frame.Request && frame.ID > 0 {
		body, _ := protocol.ErrorBytes(frame.ID, code, nil)
		if s.wire.Finish(body, supersedeWriteBound) {
			return
		}
	}
	s.wire.Close()
}

func (s *session) result(frame protocol.Frame, value any) {
	body, err := protocol.ResultBytes(frame.ID, frame.Method, value)
	if err != nil || !s.wire.Send(body) {
		s.wire.Close()
	}
}

func (s *session) error(frame protocol.Frame, code int, data any) {
	body, err := protocol.ErrorBytes(frame.ID, code, data)
	if err != nil || !s.wire.Send(body) {
		s.wire.Close()
	}
}

func (s *session) supersede() {
	if s.stopping {
		return
	}
	s.stopping = true
	s.detachRequests(protocol.Superseded)
	s.nextOut++
	body, err := protocol.RequestBytes(s.nextOut, "session.superseded", struct{}{})
	if err != nil || !s.wire.Finish(body, supersedeWriteBound) {
		s.wire.Close()
	}
}

func (s *session) await(requestID int64, leg int, reply <-chan answer, lifetime <-chan struct{}) {
	s.owned++
	s.daemon.group.Add(1)
	go func() {
		defer s.daemon.group.Done()
		value := answer{code: protocol.NotConnected}
		select {
		case value = <-reply:
		case <-lifetime:
		}
		s.inbox <- replyEvent{requestID: requestID, leg: leg, answer: value}
	}()
}
