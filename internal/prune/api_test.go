package prune

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeDoer struct {
	reqs   []*http.Request
	status map[string]int
	body   map[string]string
}

func (f *fakeDoer) Do(r *http.Request) (*http.Response, error) {
	f.reqs = append(f.reqs, r)
	code, ok := f.status[r.URL.Path]
	if !ok {
		code = 200
	}
	b, ok := f.body[r.URL.Path]
	if !ok {
		b = `{"SpaceReclaimed":100}`
	}
	return &http.Response{StatusCode: code, Status: http.StatusText(code), Body: io.NopCloser(strings.NewReader(b))}, nil
}

func TestRunAPIMirrorsThePlan(t *testing.T) {
	f := &fakeDoer{}
	res := RunAPI(t.Context(), f, Options{All: true, Until: 168 * time.Hour, BuildCache: true, Volumes: true})
	var paths []string
	for _, r := range f.reqs {
		if r.Method != http.MethodPost {
			t.Errorf("%s used %s, want POST", r.URL.Path, r.Method)
		}
		paths = append(paths, r.URL.Path)
	}
	want := "/v1.44/containers/prune /v1.44/images/prune /v1.44/volumes/prune /v1.44/build/prune"
	if strings.Join(paths, " ") != want {
		t.Errorf("paths = %v", paths)
	}
	if res.ReclaimedBytes != 400 || res.Failed != 0 {
		t.Errorf("result = %+v", res)
	}
	imgs, _ := url.QueryUnescape(f.reqs[1].URL.RawQuery)
	if !strings.Contains(imgs, `"dangling":["false"]`) || !strings.Contains(imgs, `"until":["168h"]`) {
		t.Errorf("image filters = %s", imgs)
	}
	if strings.Contains(f.reqs[2].URL.RawQuery, "until") {
		t.Errorf("volume prune has no until filter: %s", f.reqs[2].URL.RawQuery)
	}
}

func TestRunAPIDefaultImagesAreDanglingOnly(t *testing.T) {
	f := &fakeDoer{}
	RunAPI(t.Context(), f, Options{})
	if len(f.reqs) != 2 || f.reqs[1].URL.RawQuery != "" {
		t.Fatalf("want 2 unfiltered requests, got %d, query %q", len(f.reqs), f.reqs[len(f.reqs)-1].URL.RawQuery)
	}
}

func TestRunAPIFailedStepDoesNotStopTheRest(t *testing.T) {
	f := &fakeDoer{status: map[string]int{"/v1.44/containers/prune": 500}, body: map[string]string{"/v1.44/containers/prune": "boom"}}
	res := RunAPI(t.Context(), f, Options{})
	if res.Failed != 1 || len(res.Steps) != 2 || res.ReclaimedBytes != 100 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Steps[0].Err, "boom") {
		t.Errorf("error should carry the engine's message: %q", res.Steps[0].Err)
	}
}
