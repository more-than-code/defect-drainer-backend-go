// Package search is Phase A FTS envelope + optional OpenSearch REST client.
package search

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/joe/defect-drainer-go/internal/analytics"
	"github.com/joe/defect-drainer-go/internal/db"
	"github.com/joe/defect-drainer-go/internal/env"
	"github.com/joe/defect-drainer-go/internal/store"
)

// Config is OpenSearch settings. Disabled when URL is unset.
type Config struct {
	URL     string
	Index   string
	Enabled bool
}

// GetConfig reads OPENSEARCH_URL / _INDEX.
func GetConfig() Config {
	url := strings.TrimRight(env.EnvDrainer("OPENSEARCH_URL"), "/")
	index := strings.TrimSpace(env.EnvDrainer("OPENSEARCH_INDEX"))
	if index == "" {
		index = "defect-drainer-artifacts"
	}
	return Config{URL: url, Index: index, Enabled: url != ""}
}

// Hit is the multi-search envelope hit.
type Hit struct {
	ID           string   `json:"id"`
	ArtifactType string   `json:"artifact_type"`
	AppID        string   `json:"app_id"`
	DefectID     *string  `json:"defect_id"`
	JobID        *string  `json:"job_id,omitempty"`
	BatchID      *string  `json:"batch_id,omitempty"`
	Title        *string  `json:"title"`
	BodyText     *string  `json:"body_text"`
	Severity     *string  `json:"severity"`
	Status       *string  `json:"status"`
	Area         *string  `json:"area"`
	PromptKey    *string  `json:"prompt_key,omitempty"`
	BlobURI      *string  `json:"blob_uri,omitempty"`
	Score        *float64 `json:"score,omitempty"`
	Snippet      *string  `json:"snippet,omitempty"`
}

// Result is MultiSearchResult.
type Result struct {
	Mode       string `json:"mode"`
	Hits       []Hit  `json:"hits"`
	Total      int    `json:"total"`
	Facets     any    `json:"facets,omitempty"`
	OpenSearch any    `json:"opensearch,omitempty"`
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// MultiSearch prefers OpenSearch when enabled, else FTS/LIKE/recent.
func MultiSearch(sqlDB *sql.DB, q, appID, artifactType, severity, area, status, prefer string, limit int) Result {
	cfg := GetConfig()
	if limit < 1 {
		limit = 40
	}
	if prefer == "" {
		prefer = "opensearch"
	}
	if cfg.Enabled && prefer != "fts" {
		res, err := osSearch(cfg, q, appID, artifactType, severity, area, status, limit)
		if err == nil {
			return res
		}
		fbHits, mode := analytics.SearchDefects(sqlDB, q, appID, limit)
		msg := err.Error()
		return Result{
			Mode:  mode,
			Hits:  mapHits(fbHits),
			Total: len(fbHits),
			OpenSearch: map[string]any{
				"url": cfg.URL, "index": cfg.Index, "ok": false, "error": msg,
			},
		}
	}
	fbHits, mode := analytics.SearchDefects(sqlDB, q, appID, limit)
	var osInfo any
	if cfg.Enabled {
		osInfo = map[string]any{"url": cfg.URL, "index": cfg.Index, "ok": false, "error": "not queried"}
	}
	return Result{Mode: mode, Hits: mapHits(fbHits), Total: len(fbHits), OpenSearch: osInfo}
}

func mapHits(in []analytics.DefectHit) []Hit {
	out := make([]Hit, 0, len(in))
	for _, h := range in {
		id := h.ID
		out = append(out, Hit{
			ID:           h.ID,
			ArtifactType: "defect_summary",
			AppID:        h.AppID,
			DefectID:     &id,
			Title:        strPtr(h.Title),
			BodyText:     strPtr(h.Summary),
			Severity:     strPtr(h.Severity),
			Status:       strPtr(h.Status),
			Area:         strPtr(h.Area),
			Snippet:      strPtr(h.Snippet),
		})
	}
	return out
}

func osSearch(cfg Config, q, appID, artifactType, severity, area, status string, limit int) (Result, error) {
	filter := []map[string]any{}
	if appID != "" {
		filter = append(filter, map[string]any{"term": map[string]any{"app_id": appID}})
	}
	if artifactType != "" {
		filter = append(filter, map[string]any{"term": map[string]any{"artifact_type": artifactType}})
	}
	if severity != "" {
		filter = append(filter, map[string]any{"term": map[string]any{"severity": severity}})
	}
	if area != "" {
		filter = append(filter, map[string]any{"term": map[string]any{"area": area}})
	}
	if status != "" {
		filter = append(filter, map[string]any{"term": map[string]any{"status": status}})
	}
	var must []map[string]any
	q = strings.TrimSpace(q)
	if q != "" {
		must = []map[string]any{{"multi_match": map[string]any{
			"query": q, "fields": []string{"title^3", "body_text", "prompt_key^2"},
			"type": "best_fields", "fuzziness": "AUTO",
		}}}
	} else {
		must = []map[string]any{{"match_all": map[string]any{}}}
	}
	body := map[string]any{
		"size": limit,
		"query": map[string]any{"bool": map[string]any{"must": must, "filter": filter}},
		"highlight": map[string]any{
			"fields":        map[string]any{"title": map[string]any{}, "body_text": map[string]any{}},
			"pre_tags":      []string{"["},
			"post_tags":     []string{"]"},
			"fragment_size": 120,
		},
		"aggs": map[string]any{
			"artifact_type": map[string]any{"terms": map[string]any{"field": "artifact_type", "size": 20}},
			"severity":      map[string]any{"terms": map[string]any{"field": "severity", "size": 20}},
			"area":          map[string]any{"terms": map[string]any{"field": "area", "size": 30}},
			"status":        map[string]any{"terms": map[string]any{"field": "status", "size": 20}},
		},
	}
	raw, statusCode, err := osDo(cfg.URL, http.MethodPost, "/"+cfg.Index+"/_search", body, 12*time.Second)
	if err != nil {
		return Result{}, err
	}
	if statusCode >= 300 {
		return Result{}, errStatus(statusCode, raw)
	}
	var parsed struct {
		Hits struct {
			Total any `json:"total"`
			Hits  []struct {
				ID     string          `json:"_id"`
				Score  *float64        `json:"_score"`
				Source map[string]any  `json:"_source"`
				HL     map[string][]string `json:"highlight"`
			} `json:"hits"`
		} `json:"hits"`
		Aggs map[string]struct {
			Buckets []struct {
				Key   any `json:"key"`
				Count int `json:"doc_count"`
			} `json:"buckets"`
		} `json:"aggregations"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Result{}, err
	}
	total := 0
	switch t := parsed.Hits.Total.(type) {
	case float64:
		total = int(t)
	case map[string]any:
		if v, ok := t["value"].(float64); ok {
			total = int(v)
		}
	}
	hits := make([]Hit, 0, len(parsed.Hits.Hits))
	for _, h := range parsed.Hits.Hits {
		s := h.Source
		if s == nil {
			s = map[string]any{}
		}
		var snip *string
		if h.HL != nil {
			if v := h.HL["body_text"]; len(v) > 0 {
				snip = &v[0]
			} else if v := h.HL["title"]; len(v) > 0 {
				snip = &v[0]
			}
		}
		hits = append(hits, Hit{
			ID:           h.ID,
			ArtifactType: strOf(s["artifact_type"]),
			AppID:        strOf(s["app_id"]),
			DefectID:     optStr(s["defect_id"]),
			JobID:        optStr(s["job_id"]),
			BatchID:      optStr(s["batch_id"]),
			Title:        optStr(s["title"]),
			BodyText:     optStr(s["body_text"]),
			Severity:     optStr(s["severity"]),
			Status:       optStr(s["status"]),
			Area:         optStr(s["area"]),
			PromptKey:    optStr(s["prompt_key"]),
			BlobURI:      optStr(s["blob_uri"]),
			Score:        h.Score,
			Snippet:      snip,
		})
	}
	facets := map[string]any{}
	for _, name := range []string{"artifact_type", "severity", "area", "status"} {
		var buckets []map[string]any
		for _, b := range parsed.Aggs[name].Buckets {
			buckets = append(buckets, map[string]any{"key": strOf(b.Key), "count": b.Count})
		}
		facets[name] = buckets
	}
	return Result{
		Mode:   "opensearch",
		Hits:   hits,
		Total:  total,
		Facets: facets,
		OpenSearch: map[string]any{"url": cfg.URL, "index": cfg.Index, "ok": true},
	}, nil
}

func strOf(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	if s != "" {
		return s
	}
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}

func optStr(v any) *string {
	s := strOf(v)
	if s == "" {
		return nil
	}
	return &s
}

func errStatus(code int, raw []byte) error {
	msg := string(raw)
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return &osErr{msg: "search " + itoa(code) + ": " + msg}
}

type osErr struct{ msg string }

func (e *osErr) Error() string { return e.msg }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func osDo(base, method, path string, body any, timeout time.Duration) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: timeout}
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	return raw, res.StatusCode, err
}

// Ping returns true if the cluster answers.
func Ping(url string) bool {
	_, code, err := osDo(url, http.MethodGet, "/", nil, 12*time.Second)
	return err == nil && code < 500
}

// ReindexAll is a best-effort stub that 400s when disabled (handler checks).
func ReindexAll(sqlDB *sql.DB, st *store.Store) (map[string]any, error) {
	cfg := GetConfig()
	if !cfg.Enabled {
		return nil, errDisabled
	}
	// Minimal: publish defect summaries from list.
	list, err := st.List(struct {
		Status string
		Bucket string
		AppID  string
	}{Bucket: "all"})
	if err != nil {
		return nil, err
	}
	n := 0
	for _, d := range list {
		doc := map[string]any{
			"artifact_type": "defect_summary",
			"app_id":        d.AppID,
			"defect_id":     d.ID,
			"title":         d.Title,
			"body_text":     d.Summary,
			"severity":      d.Severity,
			"status":        d.Status,
			"area":          d.Area,
			"created_at":    d.CreatedAt,
		}
		_, code, err := osDo(cfg.URL, http.MethodPut, "/"+cfg.Index+"/_doc/"+d.ID, doc, 30*time.Second)
		if err == nil && code < 300 {
			n++
		}
	}
	return map[string]any{"indexed": n, "total": len(list)}, nil
}

var errDisabled = &osErr{msg: "OpenSearch disabled (set DEFECT_DRAINER_OPENSEARCH_URL)"}

// DisabledErr is the TS reindex 400 message.
func DisabledErr() error { return errDisabled }

// FlushOutbox is fail-soft; reports remaining depth.
func FlushOutbox(countRemaining int) map[string]any {
	return map[string]any{"flushed": 0, "remaining": countRemaining}
}

// EnqueueOutbox writes a search_outbox row when a publish is attempted.
func EnqueueOutbox(exec func(q string, args ...any) error, docID, op, bodyJSON, lastErr string) {
	ts := db.NowIso()
	_ = exec(`INSERT INTO search_outbox (id, doc_id, op, body_json, attempts, last_error, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`, "ob_"+ts, docID, op, bodyJSON, 1, lastErr, ts, ts)
}
