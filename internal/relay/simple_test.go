package relay

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

const simplePath = "simple/six/"

func simpleHTML(prefix string) []byte {
	return []byte(`<!DOCTYPE html><html><body><h1>Links for six</h1>` +
		`<a href="` + prefix + `b7/ce/six-1.16.0-py2.py3-none-any.whl#sha256=abc">six-1.16.0-py2.py3-none-any.whl</a><br/>` +
		`<a href="` + prefix + `71/39/six-1.16.0.tar.gz#sha256=def">six-1.16.0.tar.gz</a><br/>` +
		`</body></html>`)
}

func simpleJSON(prefix string) []byte {
	return []byte(`{"meta":{"api-version":"1.1"},"name":"six","files":[` +
		`{"filename":"six-1.16.0-py2.py3-none-any.whl","url":"` + prefix + `b7/ce/six-1.16.0-py2.py3-none-any.whl","hashes":{"sha256":"abc"}},` +
		`{"filename":"six-1.16.0.tar.gz","url":"` + prefix + `71/39/six-1.16.0.tar.gz","hashes":{"sha256":"def"}}]}`)
}

func simpleConfig(mirrors ...*fakeMirror) Config {
	upstreams := make([]Upstream, 0, len(mirrors))
	for i, mirror := range mirrors {
		upstreams = append(upstreams, simpleUpstream(mirror, string(rune('a'+i))))
	}
	return Config{Upstreams: map[Route][]Upstream{RouteSimple: upstreams}}
}

func TestSimple_RewritesPackagesPrefix(t *testing.T) {
	mirror := newFakeMirror(t, false)
	prefix := mirror.url() + "/packages/"
	mirror.put(simplePath, simpleHTML(prefix))
	mirror.behave(simplePath, mirrorBehavior{
		contentType: "text/html; charset=utf-8",
		jsonBody:    simpleJSON(prefix),
	})
	fixture := startFixture(t, fixtureOptions{cfg: simpleConfig(mirror)}, mirror)
	relayPrefix := fixture.server.BaseURL() + "/packages/"

	tests := []struct {
		name        string
		accept      string
		requestPath string
		wantType    string
		wantBody    []byte
	}{
		{
			name:        "html",
			accept:      "text/html",
			requestPath: "/simple/six/",
			wantType:    "text/html; charset=utf-8",
			wantBody:    simpleHTML(relayPrefix),
		},
		{
			name:        "json",
			accept:      "application/vnd.pypi.simple.v1+json",
			requestPath: "/simple/six/",
			wantType:    "application/vnd.pypi.simple.v1+json",
			wantBody:    simpleJSON(relayPrefix),
		},
		{
			name:        "html without trailing slash",
			accept:      "text/html",
			requestPath: "/simple/six",
			wantType:    "text/html; charset=utf-8",
			wantBody:    simpleHTML(relayPrefix),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := mirror.countAll()
			response, body := fixture.do(t, http.MethodGet, test.requestPath, map[string]string{"Accept": test.accept})
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.StatusCode)
			}
			if got := response.Header.Get("Content-Type"); got != test.wantType {
				t.Errorf("Content-Type = %q, want %q", got, test.wantType)
			}
			if got := response.Header.Get("Content-Length"); got != strconv.Itoa(len(test.wantBody)) {
				t.Errorf("Content-Length = %q, want %d", got, len(test.wantBody))
			}
			if string(body) != string(test.wantBody) {
				t.Errorf("body = %q\nwant %q", body, test.wantBody)
			}
			if strings.Contains(string(body), mirror.url()) {
				t.Errorf("body still references upstream %s", mirror.url())
			}
			recorded := mirror.recorded()
			if len(recorded) != before+1 {
				t.Fatalf("upstream requests = %d, want %d", len(recorded), before+1)
			}
			last := recorded[len(recorded)-1]
			if last.path != simplePath || last.accept != test.accept || last.method != http.MethodGet {
				t.Errorf("upstream request = %+v, want GET %s with Accept %q", last, simplePath, test.accept)
			}
		})
	}
}

func TestSimple_FallsBackToNextUpstream(t *testing.T) {
	failing := newFakeMirror(t, false)
	failing.put(simplePath, simpleHTML(failing.url()+"/packages/"))
	failing.behave(simplePath, mirrorBehavior{status: http.StatusInternalServerError, contentType: "text/html"})
	missing := newFakeMirror(t, false)
	healthy := newFakeMirror(t, false)
	healthy.put(simplePath, simpleHTML(healthy.url()+"/packages/"))
	healthy.behave(simplePath, mirrorBehavior{contentType: "text/html"})
	fixture := startFixture(t, fixtureOptions{cfg: simpleConfig(failing, missing, healthy)}, failing, missing, healthy)

	response, body := fixture.do(t, http.MethodGet, "/simple/six/", map[string]string{"Accept": "text/html"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	want := string(simpleHTML(fixture.server.BaseURL() + "/packages/"))
	if string(body) != want {
		t.Errorf("body = %q\nwant %q", body, want)
	}
	for _, mirror := range []*fakeMirror{failing, missing, healthy} {
		if got := mirror.count(simplePath); got != 1 {
			t.Errorf("mirror %s requests = %d, want 1", mirror.url(), got)
		}
	}

	response, _ = fixture.do(t, http.MethodGet, "/simple/absent/", map[string]string{"Accept": "text/html"})
	if response.StatusCode != http.StatusBadGateway {
		t.Errorf("status for index missing everywhere = %d, want 502", response.StatusCode)
	}
}
