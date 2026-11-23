package provider

import (
	"context"
	"errors"
	"sync"
)

// ErrNoActiveTurn is returned when a steering request targets a dispatch that
// does not currently have an active provider turn.
var ErrNoActiveTurn = errors.New("no active provider turn")

type activeTurnRegistry struct {
	mu    sync.Mutex
	turns map[int64]*activeTurn
}

type activeTurn struct {
	steer func(ctx context.Context, content string) error
}

func (r *activeTurnRegistry) register(dispatchID int64, steer func(context.Context, string) error) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.turns == nil {
		r.turns = make(map[int64]*activeTurn)
	}
	turn := &activeTurn{steer: steer}
	r.turns[dispatchID] = turn
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.turns[dispatchID] == turn {
			delete(r.turns, dispatchID)
		}
	}
}

func (r *activeTurnRegistry) steer(ctx context.Context, dispatchID int64, content string) error {
	r.mu.Lock()
	turn := r.turns[dispatchID]
	r.mu.Unlock()
	if turn == nil {
		return ErrNoActiveTurn
	}
	return turn.steer(ctx, content)
}

type steerRequest struct {
	content string
	result  chan error
}

type turnOutcome struct {
	result Result
	err    error
}

// runSteerable runs a provider turn and cooperatively restarts it in the same
// session when a steering request arrives. The first cancelled turn's partial
// result is discarded; the final result is the last steered turn.
func runSteerable(
	ctx context.Context,
	req Request,
	emit func(Event),
	registry *activeTurnRegistry,
	runTurn func(context.Context, Request, string, func(Event), func(error)) (Result, error),
) (Result, error) {
	if req.DispatchID <= 0 {
		return runTurn(ctx, req, req.Prompt, emit, func(error) {})
	}

	steerCh := make(chan steerRequest, 1)
	done := make(chan struct{})
	unregister := registry.register(req.DispatchID, func(steerCtx context.Context, content string) error {
		request := steerRequest{content: content, result: make(chan error, 1)}
		select {
		case steerCh <- request:
		case <-done:
			return ErrNoActiveTurn
		case <-steerCtx.Done():
			return steerCtx.Err()
		}
		select {
		case err := <-request.result:
			return err
		case <-done:
			return ErrNoActiveTurn
		case <-steerCtx.Done():
			return steerCtx.Err()
		}
	})
	defer func() {
		close(done)
		unregister()
	}()

	prompt := req.Prompt
	var pendingSteer *steerRequest
	for {
		turnCtx, cancel := context.WithCancel(ctx)
		started := make(chan error, 1)
		turnDone := make(chan turnOutcome, 1)
		go func() {
			result, err := runTurn(turnCtx, req, prompt, emit, func(err error) {
				select {
				case started <- err:
				default:
				}
			})
			turnDone <- turnOutcome{result: result, err: err}
		}()

		var startErr error
		var startedOK bool
		var outcome turnOutcome
		var outcomeReady bool

		// Prefer a start signal when it races with completion. A very fast
		// replacement turn can publish both signals before this select runs; the
		// steering request must still be acknowledged when that turn did start.
		select {
		case startErr = <-started:
			startedOK = true
		default:
		}
		if !startedOK {
			select {
			case startErr = <-started:
				startedOK = true
			case outcome = <-turnDone:
				outcomeReady = true
				select {
				case startErr = <-started:
					startedOK = true
				default:
				}
			case <-ctx.Done():
				cancel()
				outcome = <-turnDone
				if pendingSteer != nil {
					pendingSteer.result <- ctx.Err()
					pendingSteer = nil
				}
				return outcome.result, ctx.Err()
			}
		}
		if outcomeReady {
			if pendingSteer != nil {
				if startedOK && startErr == nil {
					pendingSteer.result <- nil
				} else if startedOK {
					pendingSteer.result <- startErr
				} else {
					pendingSteer.result <- errors.New("provider turn ended before steering submission")
				}
				pendingSteer = nil
			}
			cancel()
			if startedOK && startErr != nil {
				return outcome.result, startErr
			}
			return outcome.result, outcome.err
		}
		if startErr != nil {
			cancel()
			outcome = <-turnDone
			if pendingSteer != nil {
				pendingSteer.result <- startErr
				pendingSteer = nil
			}
			return outcome.result, startErr
		}

		if pendingSteer != nil {
			pendingSteer.result <- nil
			pendingSteer = nil
		}

		select {
		case steer := <-steerCh:
			cancel()
			outcome := <-turnDone
			if outcome.result.SessionID != "" {
				req.SessionID = outcome.result.SessionID
			}
			prompt = steer.content
			pendingSteer = &steer
			continue
		case outcome := <-turnDone:
			cancel()
			return outcome.result, outcome.err
		case <-ctx.Done():
			cancel()
			outcome := <-turnDone
			return outcome.result, ctx.Err()
		}
	}
}
