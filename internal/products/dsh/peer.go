package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/component"
	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/socketpath"
)

const (
	EnvComponentSocket  = "AGENT_SESSIONS_COMPONENT_SOCKET"
	EnvProductID        = "AGENT_SESSIONS_PRODUCT_ID"
	EnvAttachmentID     = "AGENT_SESSIONS_ATTACHMENT_ID"
	EnvComponentVersion = "AGENT_SESSIONS_COMPONENT_VERSION"
)

type ComponentSender interface {
	Send(bindingID string, frameType component.FrameType, operationID string, payload any) error
}

type CordisSession struct {
	Binding      component.BindingView
	NativeID     string
	Cwd          string
	NativeName   string
	Status       string
	ProductSeq   uint64
	LastTurnKind string
}

type deliveryResult struct {
	acceptance productruntime.NativeAcceptance
	err        error
}

// CordisGateway routes only the sessions on live component connections.
type CordisGateway struct {
	sender ComponentSender
	now    func() time.Time

	mu           sync.Mutex
	byBinding    map[string]CordisSession
	byAttachment map[string]string
	pending      map[string]chan deliveryResult
}

func NewCordisGateway(sender ComponentSender, now func() time.Time) (*CordisGateway, error) {
	if sender == nil {
		return nil, errors.New("DSH Cordis gateway requires a component sender")
	}
	if now == nil {
		now = time.Now
	}
	return &CordisGateway{
		sender: sender, now: now, byBinding: make(map[string]CordisSession),
		byAttachment: make(map[string]string), pending: make(map[string]chan deliveryResult),
	}, nil
}

func (gateway *CordisGateway) HandleComponentFrame(ctx context.Context, binding component.BindingView, frame component.Frame) error { //nolint:gocyclo
	_ = ctx // Durable validation/admission has already happened centrally.
	if binding.ProductID != ProductID || binding.BindingID == "" || binding.AttachmentID == "" || binding.Generation == 0 {
		return fmt.Errorf("%w: DSH component binding is incomplete", productruntime.ErrUnauthorized)
	}
	switch frame.Type {
	case component.TypeSessionAnnounce:
		var announce component.SessionAnnounce
		if err := frame.PayloadInto(&announce); err != nil {
			return err
		}
		if announce.BindingID != binding.BindingID {
			return fmt.Errorf("%w: DSH session announcement changed binding", productruntime.ErrUnauthorized)
		}
		gateway.mu.Lock()
		defer gateway.mu.Unlock()
		if prior, ok := gateway.byBinding[binding.BindingID]; ok {
			if prior.NativeID != announce.NativeSessionID {
				return fmt.Errorf("%w: DSH binding changed native session", productruntime.ErrAmbiguousSession)
			}
			if announce.ProductEventSeq < prior.ProductSeq {
				return fmt.Errorf("%w: DSH announcement sequence moved backward", productruntime.ErrStale)
			}
			if announce.ProductEventSeq == prior.ProductSeq {
				if prior.Cwd == announce.Cwd && prior.NativeName == announce.NativeName && prior.Binding == binding {
					return nil
				}
				return fmt.Errorf("%w: DSH announcement replay changed its body", productruntime.ErrStale)
			}
			prior.Cwd, prior.NativeName, prior.ProductSeq = announce.Cwd, announce.NativeName, announce.ProductEventSeq
			gateway.byBinding[binding.BindingID] = prior
			return nil
		}
		if other := gateway.byAttachment[binding.AttachmentID]; other != "" && other != binding.BindingID {
			prior, exists := gateway.byBinding[other]
			if !exists || prior.Binding.Generation >= binding.Generation {
				return fmt.Errorf("%w: DSH attachment has two current component owners", productruntime.ErrAmbiguousSession)
			}
			// Central Authority has already durably admitted this successor. Drop
			// only the obsolete generation-local projection; no authority moves
			// through this observer.
			delete(gateway.byBinding, other)
		}
		gateway.byBinding[binding.BindingID] = CordisSession{
			Binding: binding, NativeID: announce.NativeSessionID, Cwd: announce.Cwd,
			NativeName: announce.NativeName, Status: "idle", ProductSeq: announce.ProductEventSeq,
		}
		gateway.byAttachment[binding.AttachmentID] = binding.BindingID
		return nil
	case component.TypeSessionRebind:
		var rebind component.SessionRebind
		if err := frame.PayloadInto(&rebind); err != nil {
			return err
		}
		gateway.mu.Lock()
		defer gateway.mu.Unlock()
		session, ok := gateway.byBinding[binding.BindingID]
		if !ok || rebind.BindingID != binding.BindingID || rebind.OldNativeSessionID != session.NativeID || rebind.ProductEventSeq <= session.ProductSeq {
			return fmt.Errorf("%w: DSH session rebind lacks exact predecessor evidence", productruntime.ErrStale)
		}
		session.NativeID, session.ProductSeq = rebind.NewNativeSessionID, rebind.ProductEventSeq
		gateway.byBinding[binding.BindingID] = session
		return nil
	case component.TypeSessionRename:
		var rename component.SessionRename
		if err := frame.PayloadInto(&rename); err != nil {
			return err
		}
		return gateway.updateSession(binding, rename.NativeSessionID, rename.ProductEventSeq, func(session *CordisSession) {
			session.NativeName = rename.NativeName
		})
	case component.TypeSessionState:
		var state component.SessionState
		if err := frame.PayloadInto(&state); err != nil {
			return err
		}
		return gateway.updateSession(binding, state.NativeSessionID, state.ProductEventSeq, func(session *CordisSession) {
			session.Status = state.State
		})
	case component.TypeTurnEvent:
		var event component.TurnEvent
		if err := frame.PayloadInto(&event); err != nil {
			return err
		}
		return gateway.updateSession(binding, event.NativeSessionID, event.EventSeq, func(session *CordisSession) {
			session.LastTurnKind = event.Kind
			if event.Kind == "settled" || event.Kind == "failed" || event.Kind == "cancelled" {
				session.Status = "idle"
			}
		})
	case component.TypeSessionClose:
		var closed component.SessionClose
		if err := frame.PayloadInto(&closed); err != nil {
			return err
		}
		gateway.mu.Lock()
		defer gateway.mu.Unlock()
		session, ok := gateway.byBinding[binding.BindingID]
		if !ok || session.NativeID != closed.NativeSessionID {
			return fmt.Errorf("%w: DSH close changed native identity", productruntime.ErrStale)
		}
		delete(gateway.byBinding, binding.BindingID)
		delete(gateway.byAttachment, binding.AttachmentID)
		return nil
	case component.TypeDeliveryAccept:
		var accepted component.DeliveryAccept
		if err := frame.PayloadInto(&accepted); err != nil {
			return err
		}
		return gateway.finishDelivery(binding, accepted.DeliveryID, accepted.NativeSessionID, deliveryResult{acceptance: productruntime.NativeAcceptance{
			NativeSessionID: accepted.NativeSessionID, NativeMessageID: accepted.NativeMessageID,
			AcceptedAt: componentTime(accepted.AcceptedAt),
		}})
	case component.TypeDeliveryReject:
		var rejected component.DeliveryReject
		if err := frame.PayloadInto(&rejected); err != nil {
			return err
		}
		return gateway.finishDelivery(binding, rejected.DeliveryID, "", deliveryResult{err: componentCategoryError(rejected.Category, rejected.Detail)})
	case component.TypeToolCall, component.TypeToolCancel:
		// Shared tool operations are handled by the daemon.
		return nil
	default:
		return fmt.Errorf("%w: DSH observer received non-component direction %s", productruntime.ErrProtocol, frame.Type)
	}
}

func (gateway *CordisGateway) updateSession(binding component.BindingView, nativeID string, sequence uint64, apply func(*CordisSession)) error {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	session, ok := gateway.byBinding[binding.BindingID]
	if !ok || session.Binding != binding || session.NativeID != nativeID {
		return fmt.Errorf("%w: DSH component event changed session identity", productruntime.ErrStale)
	}
	if sequence <= session.ProductSeq {
		return fmt.Errorf("%w: DSH component event sequence did not advance", productruntime.ErrStale)
	}
	apply(&session)
	session.ProductSeq = sequence
	gateway.byBinding[binding.BindingID] = session
	return nil
}

func (gateway *CordisGateway) finishDelivery(binding component.BindingView, deliveryID, nativeID string, result deliveryResult) error {
	gateway.mu.Lock()
	session, ok := gateway.byBinding[binding.BindingID]
	pending := gateway.pending[deliveryID]
	if !ok || session.Binding != binding {
		gateway.mu.Unlock()
		return fmt.Errorf("%w: DSH delivery result changed exact component binding", productruntime.ErrStale)
	}
	if ok && nativeID != "" && session.NativeID != nativeID {
		gateway.mu.Unlock()
		return fmt.Errorf("%w: DSH delivery acceptance changed native identity", productruntime.ErrAmbiguousSession)
	}
	if pending != nil {
		delete(gateway.pending, deliveryID)
	}
	gateway.mu.Unlock()
	if pending == nil {
		// Central Authority has already durably admitted this result. A product
		// observer rebuilt after restart, or a caller that timed out locally,
		// may have no generation-local waiter; that miss must not roll back the
		// durable shared acceptance.
		return nil
	}
	pending <- result
	return nil
}

func (gateway *CordisGateway) SessionByBinding(bindingID string) (CordisSession, bool) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	session, ok := gateway.byBinding[bindingID]
	return session, ok
}

func (gateway *CordisGateway) SessionByAttachment(attachmentID string) (CordisSession, bool) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	bindingID := gateway.byAttachment[attachmentID]
	session, ok := gateway.byBinding[bindingID]
	return session, ok
}

func (gateway *CordisGateway) Deliver(ctx context.Context, attachmentID, nativeID string, request productruntime.DeliveryRequest) (productruntime.NativeAcceptance, error) {
	if request.DeliveryID == "" || len(request.Body) == 0 {
		return productruntime.NativeAcceptance{}, fmt.Errorf("%w: DSH delivery is incomplete", productruntime.ErrNativeRejected)
	}
	if request.Mode != productruntime.DeliveryIdleWake && request.Mode != productruntime.DeliveryBusySteer && request.Mode != productruntime.DeliveryBusyFollow {
		return productruntime.NativeAcceptance{}, fmt.Errorf("%w: DSH delivery mode %q", productruntime.ErrNativeRejected, request.Mode)
	}
	body, err := json.Marshal(map[string]string{"text": string(request.Body)})
	if err != nil {
		return productruntime.NativeAcceptance{}, fmt.Errorf("%w: encode DSH delivery: %v", productruntime.ErrProtocol, err)
	}
	gateway.mu.Lock()
	session, ok := gateway.byBinding[gateway.byAttachment[attachmentID]]
	if !ok || session.NativeID != nativeID || session.Binding.AttachmentID != attachmentID {
		gateway.mu.Unlock()
		return productruntime.NativeAcceptance{}, fmt.Errorf("%w: DSH component session is unavailable", productruntime.ErrStale)
	}
	if _, exists := gateway.pending[request.DeliveryID]; exists {
		gateway.mu.Unlock()
		return productruntime.NativeAcceptance{}, fmt.Errorf("%w: duplicate DSH delivery operation", productruntime.ErrAmbiguousSession)
	}
	response := make(chan deliveryResult, 1)
	gateway.pending[request.DeliveryID] = response
	gateway.mu.Unlock()

	err = gateway.sender.Send(session.Binding.BindingID, component.TypeDeliveryPresent, request.DeliveryID, component.DeliveryPresent{
		DeliveryID: request.DeliveryID, ReceiptID: request.ReceiptID, Mode: string(request.Mode), Body: body,
	})
	if err != nil {
		gateway.mu.Lock()
		delete(gateway.pending, request.DeliveryID)
		gateway.mu.Unlock()
		return productruntime.NativeAcceptance{}, fmt.Errorf("%w: send DSH component delivery: %v", productruntime.ErrUnavailable, err)
	}
	select {
	case <-ctx.Done():
		gateway.mu.Lock()
		delete(gateway.pending, request.DeliveryID)
		gateway.mu.Unlock()
		return productruntime.NativeAcceptance{}, fmt.Errorf("%w: DSH component delivery wait: %v", productruntime.ErrTimedOut, ctx.Err())
	case result := <-response:
		return result.acceptance, result.err
	}
}

func componentTime(value int64) time.Time {
	if value > 10_000_000_000 {
		return time.UnixMilli(value).UTC()
	}
	return time.Unix(value, 0).UTC()
}

func componentCategoryError(category component.Category, detail string) error {
	base := productruntime.ErrNativeRejected
	switch category {
	case component.CategoryUnauthorized:
		base = productruntime.ErrUnauthorized
	case component.CategoryProtocol, component.CategoryInvalidFrame, component.CategoryUnknownType, component.CategoryUnsupportedVersion:
		base = productruntime.ErrProtocol
	}
	return fmt.Errorf("%w: %s", base, productruntime.NewRedactedString(detail))
}

type MessageDriver struct{ Gateway *CordisGateway }

func NewMessageDriver(gateway *CordisGateway) (*MessageDriver, error) {
	if gateway == nil {
		return nil, errors.New("DSH message driver requires Cordis gateway")
	}
	return &MessageDriver{Gateway: gateway}, nil
}

func (driver *MessageDriver) Deliver(ctx context.Context, attachment daemon.ManagedAttachment, request productruntime.DeliveryRequest) (productruntime.NativeAcceptance, error) {
	if attachment.Product != ProductID || attachment.ID == "" || attachment.NativeSessionID == "" || attachment.State != "attached" {
		return productruntime.NativeAcceptance{}, fmt.Errorf("%w: DSH attachment is not exactly active", productruntime.ErrStale)
	}
	return driver.Gateway.Deliver(ctx, attachment.ID, attachment.NativeSessionID, request)
}

type PeerConfig struct {
	Executable      string
	DSHHome         string
	ComponentSocket string
	AllowedRoots    []string
	Gateway         *CordisGateway
	TupleVerifier   TupleVerifier
}

type preparedPeerLaunch struct {
	profile string
	cwd     string
}

type PeerDriver struct {
	config PeerConfig

	preparedMu sync.Mutex
	prepared   map[string]preparedPeerLaunch
}

func NewPeerDriver(config PeerConfig) (*PeerDriver, error) {
	if config.Executable == "" {
		config.Executable = "dsh"
	}
	if filepath.Base(config.Executable) != "dsh" || config.TupleVerifier == nil {
		return nil, errors.New("DSH peer requires the DSH executable and exact tuple verifier")
	}
	if err := validateManagedDSHHomeShape(config.DSHHome); err != nil {
		return nil, err
	}
	if err := validateComponentSocketShape(config.ComponentSocket, config.AllowedRoots); err != nil {
		return nil, err
	}
	return &PeerDriver{config: config, prepared: make(map[string]preparedPeerLaunch)}, nil
}

func (driver *PeerDriver) BuildLaunch(ctx context.Context, request productruntime.PeerLaunchRequest) (productruntime.NativeCommand, error) {
	staged, ok := driver.consumePrepared(request.AttachmentID)
	if !ok {
		return productruntime.NativeCommand{}, fmt.Errorf("%w: DSH launch has no exact prepared attachment profile", productruntime.ErrStale)
	}
	if request.ProductID != ProductID || request.AttachmentID == "" || !validCwd(request.Cwd) {
		return productruntime.NativeCommand{}, fmt.Errorf("%w: DSH peer launch is incomplete", productruntime.ErrNativeRejected)
	}
	if staged.cwd != request.Cwd {
		return productruntime.NativeCommand{}, fmt.Errorf("%w: DSH launch has no exact prepared attachment profile", productruntime.ErrStale)
	}
	if _, err := validateManagedProfile(driver.config.DSHHome, staged.profile); err != nil {
		return productruntime.NativeCommand{}, err
	}
	if err := validateComponentSocket(driver.config.ComponentSocket, driver.config.AllowedRoots); err != nil {
		return productruntime.NativeCommand{}, err
	}
	if _, err := verifyPinnedTuple(ctx, driver.config.TupleVerifier, staged.profile); err != nil {
		return productruntime.NativeCommand{}, err
	}
	for _, argument := range request.Args {
		if overridesDSHProfile(argument) {
			return productruntime.NativeCommand{}, fmt.Errorf("%w: DSH managed profile composition cannot be overridden", productruntime.ErrUnsupportedPolicy)
		}
	}
	replacements := map[string]string{
		EnvComponentSocket: driver.config.ComponentSocket, EnvProductID: ProductID,
		EnvAttachmentID: request.AttachmentID, EnvComponentVersion: component.ContractRevision,
		"DSH_HOME": driver.config.DSHHome,
	}
	environment := replaceEnv(request.Env, replacements)
	return productruntime.NativeCommand{
		Path: driver.config.Executable, Args: append([]string{"--profile", staged.profile}, request.Args...),
		Env: environment, Cwd: request.Cwd,
	}, nil
}

func (driver *PeerDriver) prepareAttachment(ctx context.Context, attachment daemon.ManagedAttachment) error {
	driver.preparedMu.Lock()
	defer driver.preparedMu.Unlock()
	delete(driver.prepared, attachment.ID)
	if attachment.Product != ProductID || attachment.ID == "" || validateProfileIdentity(attachment.ProfileIdentity) != nil || !validCwd(attachment.Cwd) {
		return fmt.Errorf("%w: DSH prepared attachment requires exact profile", productruntime.ErrNativeRejected)
	}
	if _, err := validateManagedProfile(driver.config.DSHHome, attachment.ProfileIdentity); err != nil {
		return err
	}
	if _, err := verifyPinnedTuple(ctx, driver.config.TupleVerifier, attachment.ProfileIdentity); err != nil {
		return err
	}
	staged := preparedPeerLaunch{profile: attachment.ProfileIdentity, cwd: attachment.Cwd}
	driver.prepared[attachment.ID] = staged
	return nil
}

func (driver *PeerDriver) consumePrepared(attachmentID string) (preparedPeerLaunch, bool) {
	driver.preparedMu.Lock()
	defer driver.preparedMu.Unlock()
	staged, ok := driver.prepared[attachmentID]
	delete(driver.prepared, attachmentID)
	return staged, ok
}

func (driver *PeerDriver) clearPrepared(attachmentID string) {
	driver.preparedMu.Lock()
	delete(driver.prepared, attachmentID)
	driver.preparedMu.Unlock()
}

func validateManagedDSHHome(path string) error {
	if err := validateManagedDSHHomeShape(path); err != nil {
		return err
	}
	canonical, err := canonicalExistingPath(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %w", productruntime.ErrUnavailable, errManagedProfileUnavailable)
	}
	if err != nil || canonical != path || temporaryPath(canonical) {
		return fmt.Errorf("%w: DSH home is missing, non-canonical, or temporary", productruntime.ErrUnsupportedPolicy)
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %w", productruntime.ErrUnavailable, errManagedProfileUnavailable)
	}
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: DSH home is not an existing directory", productruntime.ErrUnsupportedPolicy)
	}
	roots := make([]string, 0, 2)
	if home, homeErr := os.UserHomeDir(); homeErr == nil && filepath.IsAbs(home) {
		roots = append(roots, filepath.Join(home, ".local", "state", "agent-sessions"))
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(xdg) && filepath.Clean(xdg) == xdg {
		roots = append(roots, filepath.Join(xdg, "agent-sessions"))
	}
	for _, root := range roots {
		if !temporaryPath(root) && canonicalWithin(path, root) {
			return nil
		}
	}
	return fmt.Errorf("%w: DSH home is outside Agent Sessions HOME/XDG state", productruntime.ErrUnsupportedPolicy)
}

func (driver *PeerDriver) AttachmentAdapter(deps productruntime.HostDeps) (daemon.AttachmentAdapter, error) {
	if driver.config.Gateway == nil {
		return daemon.AttachmentAdapter{}, errors.New("DSH peer attachment requires Cordis gateway")
	}
	corroborate := func(ctx context.Context, attachment daemon.ManagedAttachment, observed daemon.NativeEvidence) (daemon.NativeEvidence, error) {
		if attachment.Product != ProductID || attachment.ID == "" || validateProfileIdentity(attachment.ProfileIdentity) != nil || !validCwd(attachment.Cwd) {
			return daemon.NativeEvidence{}, fmt.Errorf("%w: DSH attachment identity is invalid", productruntime.ErrUnauthorized)
		}
		if _, err := validateManagedProfile(driver.config.DSHHome, attachment.ProfileIdentity); err != nil {
			return daemon.NativeEvidence{}, err
		}
		if _, err := verifyPinnedTuple(ctx, driver.config.TupleVerifier, attachment.ProfileIdentity); err != nil {
			return daemon.NativeEvidence{}, err
		}
		session, ok := driver.config.Gateway.SessionByAttachment(attachment.ID)
		if !ok || attachment.NativeSessionID != "" && attachment.NativeSessionID != session.NativeID {
			return daemon.NativeEvidence{}, fmt.Errorf("%w: DSH component session does not match attachment", productruntime.ErrStale)
		}
		if !sameCwd(attachment.Cwd, session.Cwd) {
			return daemon.NativeEvidence{}, fmt.Errorf("%w: DSH attachment cwd changed native session", productruntime.ErrStale)
		}
		if observed.Process != (session.Binding.ProcessIdentity) && observed.Process.PID != 0 {
			return daemon.NativeEvidence{}, fmt.Errorf("%w: DSH observed process changed component owner", productruntime.ErrUnauthorized)
		}
		if deps.Processes != nil {
			observation, err := deps.Processes.ObserveIdentity(context.Background(), session.Binding.ProcessIdentity)
			if err != nil || observation.Status != procinfo.IdentityMatches {
				return daemon.NativeEvidence{}, fmt.Errorf("%w: DSH component process is not live", productruntime.ErrStale)
			}
		}
		return daemon.NativeEvidence{
			Process: session.Binding.ProcessIdentity, Executable: driver.config.Executable,
			ThreadID: session.NativeID, LeaderIdentity: session.Binding.BindingID,
		}, nil
	}
	return daemon.AttachmentAdapter{
		Prepare: func(ctx context.Context, attachment daemon.ManagedAttachment) (daemon.NativeEvidence, error) {
			if err := driver.prepareAttachment(ctx, attachment); err != nil {
				return daemon.NativeEvidence{}, err
			}
			return daemon.NativeEvidence{}, nil
		},
		Adopt: func(ctx context.Context, attachment daemon.ManagedAttachment, observed daemon.NativeEvidence) (daemon.NativeEvidence, error) {
			return corroborate(ctx, attachment, observed)
		},
		Refresh: func(ctx context.Context, attachment daemon.ManagedAttachment) (daemon.NativeEvidence, error) {
			return corroborate(ctx, attachment, attachment.Evidence)
		},
		Authorize: func(ctx context.Context, attachment daemon.ManagedAttachment, observed daemon.NativeEvidence) error {
			corroborated, err := corroborate(ctx, attachment, observed)
			if err == nil && !reflect.DeepEqual(corroborated, observed) {
				return fmt.Errorf("%w: DSH authorization evidence changed", productruntime.ErrUnauthorized)
			}
			return err
		},
		Detach: func(_ context.Context, attachment daemon.ManagedAttachment) error {
			driver.clearPrepared(attachment.ID)
			return nil
		},
		Rollback: func(_ context.Context, attachment daemon.ManagedAttachment) error {
			driver.clearPrepared(attachment.ID)
			return nil
		},
	}, nil
}

func (driver *PeerDriver) Rename(_ context.Context, attachment daemon.ManagedAttachment, name string) (productruntime.NativeName, error) {
	name = strings.TrimSpace(name)
	if attachment.Product != ProductID || name == "" || strings.ContainsAny(name, "\x00\r\n") {
		return productruntime.NativeName{}, fmt.Errorf("%w: DSH rename request is invalid", productruntime.ErrNativeRejected)
	}
	// Native Cordis remains the sole title writer. Product-originated title
	// events refresh the component projection, but the correlated
	// Agent-Sessions-to-DSH mutation callback is intentionally not wired in this
	// slice. Never substitute projcache or a daemon-side alias.
	return productruntime.NativeName{}, fmt.Errorf("%w: Agent Sessions-initiated DSH title mutation is not wired", productruntime.ErrUnsupportedRename)
}

func validateComponentSocket(path string, allowedRoots []string) error {
	if err := validateComponentSocketShape(path, allowedRoots); err != nil {
		return err
	}
	clean := filepath.Clean(path)
	canonical, err := canonicalExistingPath(clean)
	if err != nil || temporaryPath(canonical) {
		return fmt.Errorf("%w: DSH component socket parent is unavailable or temporary", productruntime.ErrUnsupportedPolicy)
	}
	home, _ := os.UserHomeDir()
	roots := []string{home, os.Getenv("XDG_STATE_HOME"), os.Getenv("XDG_RUNTIME_DIR"), os.Getenv("XDG_CONFIG_HOME")}
	if len(allowedRoots) > 0 {
		for _, allowed := range allowedRoots {
			for _, root := range roots {
				if root != "" && canonicalWithin(allowed, root) && canonicalWithin(clean, allowed) {
					return nil
				}
			}
		}
		return fmt.Errorf("%w: DSH component socket is outside Agent Sessions HOME/XDG roots", productruntime.ErrUnsupportedPolicy)
	}
	for _, root := range roots {
		if root != "" && canonicalWithin(clean, root) {
			return nil
		}
	}
	return fmt.Errorf("%w: DSH component socket is not below a HOME/XDG root", productruntime.ErrUnsupportedPolicy)
}

func validateComponentSocketShape(path string, allowedRoots []string) error {
	if filepath.Base(path) != component.ComponentSocketName || socketpath.Validate(path) != nil {
		return fmt.Errorf("%w: DSH component socket is invalid", productruntime.ErrUnsupportedPolicy)
	}
	clean := filepath.Clean(path)
	if temporaryPathLexical(clean) {
		return fmt.Errorf("%w: DSH sandbox masks /tmp component sockets", productruntime.ErrUnsupportedPolicy)
	}
	if len(allowedRoots) > 0 {
		for _, allowed := range allowedRoots {
			allowed = filepath.Clean(allowed)
			if filepath.IsAbs(allowed) && !temporaryPathLexical(allowed) && within(clean, allowed) {
				return nil
			}
		}
		return fmt.Errorf("%w: DSH component socket is outside the configured static root", productruntime.ErrUnsupportedPolicy)
	}
	return nil
}

func canonicalWithin(candidate, root string) bool {
	if !within(filepath.Clean(candidate), filepath.Clean(root)) {
		return false
	}
	canonicalCandidate, candidateErr := canonicalExistingPath(candidate)
	canonicalRoot, rootErr := canonicalExistingPath(root)
	return candidateErr == nil && rootErr == nil && !temporaryPath(canonicalRoot) && within(canonicalCandidate, canonicalRoot)
}

func temporaryPath(path string) bool {
	if path == "" {
		return false
	}
	roots := []string{"/tmp", "/private/tmp", os.TempDir()}
	for _, root := range roots {
		if root != "" && within(path, root) {
			return true
		}
		canonicalRoot, err := canonicalExistingPath(root)
		if err == nil && canonicalRoot != "" && within(path, canonicalRoot) {
			return true
		}
	}
	return false
}

// canonicalExistingPath resolves the longest existing prefix, then appends
// the still-absent suffix lexically. Component sockets may not exist yet, but
// an existing parent symlink into macOS /private/tmp must still fail closed.
func canonicalExistingPath(candidate string) (string, error) {
	candidate = filepath.Clean(candidate)
	if !filepath.IsAbs(candidate) {
		return "", errors.New("path is not absolute")
	}
	prefix := candidate
	suffix := make([]string, 0, 4)
	for {
		if _, err := os.Lstat(prefix); err == nil {
			resolved, err := filepath.EvalSymlinks(prefix)
			if err != nil {
				return "", err
			}
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			return "", os.ErrNotExist
		}
		suffix = append(suffix, filepath.Base(prefix))
		prefix = parent
	}
}

func within(path, root string) bool {
	root = filepath.Clean(root)
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func envValue(environment []productruntime.EnvVar, name string) string {
	for index := len(environment) - 1; index >= 0; index-- {
		if environment[index].Name == name {
			return environment[index].Value
		}
	}
	return ""
}

func replaceEnv(environment []productruntime.EnvVar, replacements map[string]string) []productruntime.EnvVar {
	result := make([]productruntime.EnvVar, 0, len(environment)+len(replacements))
	ordered := []string{"DSH_HOME", EnvAttachmentID, EnvComponentSocket, EnvComponentVersion, EnvProductID}
	reserved := map[string]struct{}{}
	for _, name := range ordered {
		reserved[name] = struct{}{}
	}
	for _, variable := range environment {
		if _, isReserved := reserved[variable.Name]; !isReserved {
			result = append(result, variable)
		}
	}
	for _, name := range ordered {
		if value, present := replacements[name]; present {
			result = append(result, productruntime.EnvVar{Name: name, Value: value})
		}
	}
	return result
}

func overridesDSHProfile(argument string) bool {
	return argument == "--profile" || strings.HasPrefix(argument, "--profile=") ||
		argument == "--patch" || strings.HasPrefix(argument, "--patch=") ||
		argument == "--dump-config" || argument == "--dump-default-config"
}

var _ component.Handler = (*CordisGateway)(nil)
var _ productruntime.MessageDriver = (*MessageDriver)(nil)
var _ productruntime.PeerDriver = (*PeerDriver)(nil)
