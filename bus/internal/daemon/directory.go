package daemon

import (
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/antst/agent-sessions/bus/internal/protocol"
)

type entry struct {
	row            row
	peer           bool
	info           map[string]any
	declaredGroups []string
	claimed        bool
	running        bool
	attachment     *session
	done           chan struct{}
}

type directory struct {
	daemon  *Daemon
	mu      sync.Mutex
	entries map[string]*entry
	names   map[string]*entry
	tokens  map[string]*launch
}

type view struct {
	entry              *entry
	id, name           string
	kind, product      string
	groups             []string
	info               map[string]any
	connected, running bool
}

func newDirectory(daemon *Daemon, rows []row) *directory {
	d := &directory{daemon: daemon, entries: map[string]*entry{}, names: map[string]*entry{}, tokens: map[string]*launch{}}
	for _, value := range rows {
		item := &entry{row: cloneRow(value), done: closedChannel()}
		d.entries[value.SessionID], d.names[value.Name] = item, item
	}
	return d
}

func (d *directory) installPeer(owner *session, hello *protocol.PeerHello, host string) (current *entry, displaced *session, ended *entry, ok bool) {
	id, name := qualify(hello.SessionID, host), qualify(hello.Name, host)
	d.mu.Lock()
	defer d.mu.Unlock()
	old := owner.identity
	if old != nil && old.attachment != owner {
		return nil, nil, nil, false
	}
	if old != nil && old.row.SessionID == id {
		if old.row.Product != hello.Product || !slices.Equal(old.declaredGroups, hello.Groups) {
			return nil, nil, nil, false
		}
		old.row.Name, old.info = name, maps.Clone(hello.Info)
		return old, nil, nil, true
	}
	if found := d.entries[id]; found != nil && !found.peer {
		return nil, nil, nil, false
	}
	item := &entry{peer: true, declaredGroups: append([]string(nil), hello.Groups...), attachment: owner, done: make(chan struct{})}
	item.row = row{SessionID: id, Product: hello.Product, Name: name}
	item.row.Groups = orderedPeerGroups(hello.Groups, privateGroup(item))
	item.info = maps.Clone(hello.Info)
	if old != nil {
		delete(d.entries, old.row.SessionID)
		d.end(old)
		ended = old
	}
	if found := d.entries[id]; found != nil {
		displaced = found.attachment
		d.end(found)
	}
	d.entries[id] = item
	return item, displaced, ended, true
}

func (d *directory) current(item *entry, owner *session) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return item != nil && d.entries[item.row.SessionID] == item && item.attachment == owner
}

func (d *directory) finishRun(item *entry, owner *session) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if item != nil && item.attachment == owner {
		item.running = false
	}
}

func (d *directory) detach(item *entry, owner *session) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if item == nil || item.attachment != owner {
		return
	}
	d.end(item)
	if item.peer {
		delete(d.entries, item.row.SessionID)
	} else {
		item.claimed = true
	}
}

func (d *directory) offline(item *entry, owner *session, forget bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if item.attachment == owner {
		item.attachment, item.running = nil, false
	}
	item.claimed = false
	if forget && d.entries[item.row.SessionID] == item {
		delete(d.entries, item.row.SessionID)
		delete(d.names, item.row.Name)
	}
}

func (d *directory) reserveFresh(value row, start *launch) (*entry, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.names[value.Name] != nil {
		return nil, protocol.NameTaken
	}
	if code := d.addLaunch(start); code != 0 {
		return nil, code
	}
	item := &entry{row: cloneRow(value), claimed: true, done: make(chan struct{})}
	start.entry = item
	d.names[value.Name] = item
	return item, 0
}

func (d *directory) reserveResume(id string, groups []string, start *launch) (*entry, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	previous := d.entries[id]
	if previous == nil || previous.peer || !shares(groups, previous.row.Groups) {
		return nil, protocol.UnknownSession
	}
	if previous.claimed {
		return nil, protocol.Busy
	}
	if previous.attachment != nil {
		return nil, protocol.AlreadyConnected
	}
	if code := d.addLaunch(start); code != 0 {
		return nil, code
	}
	item := &entry{row: cloneRow(previous.row), claimed: true, done: make(chan struct{})}
	d.entries[id], d.names[item.row.Name], start.entry, start.product = item, item, item, item.row.Product
	return item, 0
}

func (d *directory) reserveDescribe(start *launch) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.addLaunch(start)
}

func (d *directory) addLaunch(start *launch) int {
	if d.daemon.closing.Load() {
		return protocol.Internal
	}
	d.tokens[start.token] = start
	d.daemon.group.Add(1)
	return 0
}

func (d *directory) claimWorker(owner *session, token, product string) (*launch, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	start := d.tokens[token]
	if start == nil || start.product != product || start.owner != nil {
		return nil, false
	}
	delete(d.tokens, token)
	start.owner = owner
	return start, true
}

func (d *directory) reserveID(item *entry, id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !item.claimed || d.entries[id] != nil {
		return false
	}
	item.row.SessionID = id
	d.entries[id] = item
	return true
}

func (d *directory) publish(start *launch, owner *session, createdAt time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	item := start.entry
	if d.entries[item.row.SessionID] != item || !item.claimed || item.attachment != nil {
		return false
	}
	item.claimed = false
	item.attachment = owner
	if !createdAt.IsZero() {
		item.row.CreatedAt = createdAt
	}
	return true
}

func (d *directory) releaseLaunch(start *launch, unclaimed bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if unclaimed && (start.owner != nil || d.tokens[start.token] != start) {
		return false
	}
	if d.tokens[start.token] == start {
		delete(d.tokens, start.token)
	}
	item := start.entry
	if item == nil || !item.claimed {
		return true
	}
	item.claimed = false
	d.end(item)
	if !item.peer && item.row.CreatedAt.IsZero() {
		delete(d.names, item.row.Name)
		if d.entries[item.row.SessionID] == item {
			delete(d.entries, item.row.SessionID)
		}
	}
	return true
}

func (d *directory) visible(groups []string) []view {
	d.mu.Lock()
	defer d.mu.Unlock()
	items := make([]view, 0, len(d.entries))
	for _, item := range d.entries {
		if !visibleTo(item, groups) {
			continue
		}
		kind := "lane"
		if item.peer {
			kind = "peer"
		}
		items = append(items, view{entry: item, id: item.row.SessionID, name: item.row.Name,
			kind: kind, product: item.row.Product, groups: append([]string(nil), item.row.Groups...), info: maps.Clone(item.info), connected: item.attachment != nil, running: item.running})
	}
	sort.Slice(items, func(left, right int) bool { return items[left].id < items[right].id })
	return items
}

func (d *directory) resolveLocked(canonical string, groups []string) (*entry, bool) {
	if item := d.entries[canonical]; visibleTo(item, groups) {
		return item, false
	}
	var found *entry
	for _, item := range d.entries {
		if item.row.Name == canonical && visibleTo(item, groups) {
			if found != nil {
				return nil, true
			}
			found = item
		}
	}
	return found, false
}

func visibleTo(item *entry, groups []string) bool {
	return item != nil && (item.peer || !item.row.CreatedAt.IsZero()) && shares(groups, item.row.Groups)
}

func (d *directory) routeLabel(value, host string, groups []string, method string, request routedRequest) (*entry, bool, int) {
	canonical, code := canonicalInput(value, host)
	if code != 0 {
		return nil, false, code
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	item, ambiguous := d.resolveLocked(canonical, groups)
	if item == nil || ambiguous {
		return item, ambiguous, 0
	}
	return item, false, d.routeLocked(item, method, request)
}

func (d *directory) routeLocked(item *entry, method string, request routedRequest) int {
	if item == nil || d.entries[item.row.SessionID] != item || item.attachment == nil {
		return protocol.NotConnected
	}
	if item.peer && method != "message.deliver" {
		return protocol.UnknownSession
	}
	switch method {
	case "turn.run":
		if item.running {
			return protocol.Busy
		}
		item.running = true
	case "turn.interrupt":
		if !item.running {
			return protocol.NotRunning
		}
	case "session.close":
		if item.claimed {
			return protocol.Busy
		}
		item.claimed = true
	}
	request.destination = item
	if item.attachment.wire.Post(request) {
		return 0
	}
	if method == "turn.run" {
		item.running = false
	}
	if method == "session.close" {
		item.claimed = false
	}
	select {
	case <-item.attachment.wire.Done():
		return protocol.NotConnected
	default:
		return protocol.Busy
	}
}

func (d *directory) end(item *entry) {
	item.attachment, item.running = nil, false
	close(item.done)
}

func closedChannel() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
