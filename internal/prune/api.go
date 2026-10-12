package prune

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Doer is the slice of http.Client RunAPI needs. The supervisor hands it a
// client that dials the engine over the bridge's own transport, so the prune
// never crosses the pipe server and never counts as client activity (#496).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// apiVersion pins the engine API like busyProbe does, for the same reason: a
// bare unversioned path is ambiguous.
const apiVersion = "v1.44"

// apiResult is the part of every prune response RunAPI reads. The four
// endpoints differ only in the name of the list of things they deleted.
type apiResult struct {
	SpaceReclaimed int64 `json:"SpaceReclaimed"`
}

// RunAPI is Run for the supervisor: the same plan, executed against the engine
// API instead of the docker CLI. The CLI connects over the named pipe like any
// other client, so a scheduled prune stamped LastActivity and a short
// prune.every kept the engine from ever idling (#496). A failed step never
// stops the others, as in Run.
func RunAPI(ctx context.Context, c Doer, o Options) Result {
	var res Result
	for _, st := range Plan(o) {
		n, err := apiStep(ctx, c, st.Name, o)
		if err != nil {
			st.Err = err.Error()
			res.Failed++
		} else {
			st.ReclaimedBytes = n
			res.ReclaimedBytes += n
		}
		res.Steps = append(res.Steps, st)
	}
	return res
}

func apiStep(ctx context.Context, c Doer, name string, o Options) (int64, error) {
	filters := map[string][]string{}
	// volume prune has no until filter, as in Plan.
	if o.Until > 0 && name != "volumes" {
		filters["until"] = []string{durationArg(o.Until)}
	}
	q := url.Values{}
	var path string
	switch name {
	case "containers":
		path = "/containers/prune"
	case "images":
		path = "/images/prune"
		// The API prunes dangling images by default; -a is dangling=false.
		if o.All {
			filters["dangling"] = []string{"false"}
		}
	case "volumes":
		path = "/volumes/prune"
	case "build-cache":
		path = "/build/prune"
	default:
		return 0, fmt.Errorf("unknown prune step %q", name)
	}
	if len(filters) > 0 {
		b, err := json.Marshal(filters)
		if err != nil {
			return 0, err
		}
		q.Set("filters", string(b))
	}
	u := "http://engine/" + apiVersion + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return 0, fmt.Errorf("engine returned %s: %s", resp.Status, msg)
	}
	var r apiResult
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, fmt.Errorf("decoding %s response: %w", name, err)
	}
	return r.SpaceReclaimed, nil
}
