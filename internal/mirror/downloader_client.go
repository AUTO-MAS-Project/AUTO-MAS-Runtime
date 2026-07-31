package mirror

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

const maxRedirects = 10

var (
	errConnectionTimedOut        = errors.New("download connection timed out")
	errRequestCancelled          = errors.New("download request cancelled")
	errTransportCauseUnavailable = errors.New(
		"http transport cause is unavailable",
	)
)

type redirectError struct {
	kind FailureKind
}

type responseHandle struct {
	response *http.Response
	cancel   context.CancelFunc
}

type doResult struct {
	response *http.Response
	err      error
}

func (e *redirectError) Error() string {
	return "http redirect violates download policy"
}

func newDefaultHTTPClient() httpClient {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Go 标准库当前保证默认值为 *http.Transport；保留失败关闭的无网络 client。
		return roundTripperClient{err: errors.New("default http transport is unavailable")}
	}
	return newHTTPClientWithTransport(base.Clone())
}

func newHTTPClientWithTransport(transport *http.Transport) *http.Client {
	configured := transport.Clone()
	configured.DisableCompression = true
	return &http.Client{
		Transport:     configured,
		CheckRedirect: checkDownloadRedirect,
	}
}

type roundTripperClient struct {
	err error
}

func (c roundTripperClient) Do(*http.Request) (*http.Response, error) {
	return nil, c.err
}

func checkDownloadRedirect(request *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return &redirectError{kind: FailureURLPolicy}
	}
	if request == nil || request.URL == nil {
		return &redirectError{kind: FailureURLPolicy}
	}
	if _, err := validateHTTPSURL(request.URL.String()); err != nil {
		if strings.EqualFold(request.URL.Scheme, "http") {
			return &redirectError{kind: FailureRedirectDowngrade}
		}
		return &redirectError{kind: FailureURLPolicy}
	}
	return nil
}

func (d *Downloader) doRequest(
	operationCtx context.Context,
	target *url.URL,
) (*responseHandle, *DownloadFailure) {
	requestCtx, cancel := context.WithCancel(operationCtx)
	request, err := http.NewRequestWithContext(
		requestCtx,
		http.MethodGet,
		target.String(),
		nil,
	)
	if err != nil {
		cancel()
		return nil, newDownloadFailure(FailureInvalidRequest, 0, err)
	}
	request.Header.Set("Accept-Encoding", "identity")

	connectTimer := d.timers(d.connectTimeout)
	results := make(chan doResult, 1)
	go func() {
		response, doErr := d.client.Do(request)
		results <- doResult{response: response, err: doErr}
	}()

	var (
		result doResult
		kind   FailureKind
		cause  error
	)
	select {
	case result = <-results:
		stopAndDrainTimer(connectTimer)
		if operationCtx.Err() != nil {
			kind = FailureCancelled
			cause = errors.Join(
				errRequestCancelled,
				operationCtx.Err(),
				sanitizeTransportError(result.err),
			)
		} else if result.err != nil {
			kind = classifyTransportFailure(result.err)
			cause = sanitizeTransportError(result.err)
		}
	case <-operationCtx.Done():
		stopAndDrainTimer(connectTimer)
		cancel()
		result = <-results
		kind = FailureCancelled
		cause = errors.Join(
			errRequestCancelled,
			operationCtx.Err(),
			sanitizeTransportError(result.err),
		)
	case <-connectTimer.C():
		operationErr := operationCtx.Err()
		cancel()
		result = <-results
		if operationErr != nil {
			kind = FailureCancelled
			cause = errors.Join(
				errRequestCancelled,
				operationErr,
				sanitizeTransportError(result.err),
			)
		} else {
			kind = FailureConnectTimeout
			cause = errors.Join(
				errConnectionTimedOut,
				sanitizeTransportError(result.err),
			)
		}
	}

	if kind != "" {
		closeErr := closeResponse(result.response)
		cancel()
		return nil, newDownloadFailure(kind, 0, errors.Join(cause, closeErr))
	}
	if result.response == nil {
		cancel()
		return nil, newDownloadFailure(
			FailureNetwork,
			0,
			errors.New("http client returned no response"),
		)
	}
	finalKind, finalErr := validateFinalResponseURL(result.response)
	if finalErr != nil {
		closeErr := closeResponse(result.response)
		cancel()
		return nil, newDownloadFailure(
			finalKind,
			0,
			errors.Join(finalErr, closeErr),
		)
	}
	return &responseHandle{response: result.response, cancel: cancel}, nil
}

func classifyTransportFailure(err error) FailureKind {
	var redirect *redirectError
	if errors.As(err, &redirect) {
		return redirect.kind
	}
	return FailureNetwork
}

func sanitizeTransportError(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return safeExternalError(
			"http transport failed",
			stripURLError(urlErr.Err),
		)
	}
	return safeExternalError("http transport failed", err)
}

func stripURLError(err error) error {
	if err == nil {
		return errTransportCauseUnavailable
	}
	var nested *url.Error
	if errors.As(err, &nested) {
		return stripURLError(nested.Err)
	}
	return err
}

func validateFinalResponseURL(response *http.Response) (FailureKind, error) {
	if response.Request == nil || response.Request.URL == nil {
		return FailureURLPolicy, errors.New("http response url is unavailable")
	}
	_, err := validateHTTPSURL(response.Request.URL.String())
	if err == nil {
		return "", nil
	}
	if strings.EqualFold(response.Request.URL.Scheme, "http") {
		return FailureRedirectDowngrade, err
	}
	return FailureURLPolicy, err
}

func closeResponse(response *http.Response) error {
	if response == nil || response.Body == nil {
		return nil
	}
	if err := response.Body.Close(); err != nil {
		return safeExternalError("http response close failed", err)
	}
	return nil
}

func stopAndDrainTimer(value timer) {
	if value.Stop() {
		return
	}
	select {
	case <-value.C():
	default:
	}
}
