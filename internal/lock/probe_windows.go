//go:build windows

package lock

import (
	"context"
	"errors"
	"fmt"
)

// Probe 零等待探测指定 Mutex，且不保留临时取得的所有权。
func (s *Set) Probe(ctx context.Context, kind Kind) (ProbeResult, error) {
	if ctx == nil {
		return ProbeResult{}, errors.New("probe mutex: nil context")
	}
	if !kind.Valid() {
		return ProbeResult{}, fmt.Errorf("invalid mutex kind %q", kind)
	}
	if err := ctx.Err(); err != nil {
		return ProbeResult{}, err
	}
	return s.dispatchProbe(ctx, kind)
}

func (s *Set) dispatchProbe(
	ctx context.Context,
	kind Kind,
) (ProbeResult, error) {
	response, err := s.dispatch(ctx, workerRequest{
		operation: requestProbe,
		kind:      kind,
	})
	return response.probe, err
}

func (s *workerState) probe(request workerRequest) workerResponse {
	var result ProbeResult
	if s.poison != nil {
		return workerResponse{
			probe: result,
			err:   &PoisonedError{Cause: s.poison},
		}
	}
	if err := request.ctx.Err(); err != nil {
		return workerResponse{probe: result, err: err}
	}
	if !request.kind.Valid() {
		return workerResponse{
			probe: result,
			err:   fmt.Errorf("invalid mutex kind %q", request.kind),
		}
	}
	if s.activeKind == request.kind {
		result = ProbeResult{Held: true}
		if err := request.ctx.Err(); err != nil {
			return workerResponse{probe: result, err: err}
		}
		return workerResponse{probe: result}
	}

	slot := s.slot(request.kind)
	result, err := s.probeSlot(
		request.ctx,
		slot,
		"wait-probe",
		"release-probe",
	)
	if err != nil {
		return workerResponse{probe: result, err: err}
	}
	if err := request.ctx.Err(); err != nil {
		return workerResponse{probe: result, err: err}
	}
	return workerResponse{probe: result}
}
