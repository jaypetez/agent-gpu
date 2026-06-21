package httpapi

import (
	"context"
	"net/http"
	"sort"

	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// openAICreated is the stable "created" timestamp reported for every model on
// /v1/models. types.Model carries no creation time, and the field is required
// by the OpenAI schema, so a fixed value is used: it keeps responses
// deterministic (important for tests and caching) and avoids implying a
// freshness the data does not have. Clients that need real availability use the
// richer /models endpoint.
const openAICreated int64 = 0

// catalogModel is one aggregated, permission-passing model: a single logical
// model deduplicated across the fleet, with the workers currently serving it.
type catalogModel struct {
	Name    string
	Digest  string
	Workers []string // ids of Online workers serving this model, sorted
}

// catalog aggregates the fleet snapshot into the per-key visible model list. It
// keeps only Online workers, deduplicates models by Name, records per-model
// worker availability, and includes a model only if key may run inference
// against it — using authz.Infer so visibility matches dispatch-time
// authorization exactly. The result is sorted by model name for deterministic
// output.
func (s *Server) catalog(ctx context.Context, key store.APIKey) []catalogModel {
	// Accumulate per model name across Online workers. A model on several workers
	// collapses to one entry whose Workers list grows; the first non-empty digest
	// seen wins (workers serving the same model report the same digest).
	type agg struct {
		digest  string
		workers map[string]struct{}
	}
	byName := make(map[string]*agg)

	for _, wk := range s.fleet.Fleet() {
		if wk.Status != types.WorkerOnline {
			continue
		}
		for _, m := range wk.Models {
			a, ok := byName[m.Name]
			if !ok {
				a = &agg{workers: make(map[string]struct{})}
				byName[m.Name] = a
			}
			if a.digest == "" {
				a.digest = m.Digest
			}
			a.workers[wk.ID] = struct{}{}
		}
	}

	out := make([]catalogModel, 0, len(byName))
	for name, a := range byName {
		// Permission filter: hide a model the key cannot run inference against, so
		// the catalog never advertises a model the key would be 403'd on at
		// dispatch. Authorize audits each decision via the shared authorizer.
		if err := s.authz.Authorize(ctx, key, name, authz.Infer); err != nil {
			continue
		}
		workers := make([]string, 0, len(a.workers))
		for id := range a.workers {
			workers = append(workers, id)
		}
		sort.Strings(workers)
		out = append(out, catalogModel{Name: name, Digest: a.digest, Workers: workers})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---- OpenAI-compatible /v1/models ----

type openAIModelList struct {
	Object string        `json:"object"`
	Data   []openAIModel `json:"data"`
}

type openAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// handleOpenAIModels serves GET /v1/models in the OpenAI-canonical shape. The
// list is the per-key, Online-only, deduplicated catalog; "created" is the
// stable openAICreated sentinel.
func (s *Server) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		// Advertise the permitted method on the 405 (RFC 7231 §6.5.5).
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	key, ok := keyFromContext(r.Context())
	if !ok {
		// Unreachable behind authMiddleware; defended so a future misroute fails
		// closed rather than leaking the catalog.
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing api key")
		return
	}
	models := s.catalog(r.Context(), key)
	data := make([]openAIModel, 0, len(models))
	for _, m := range models {
		data = append(data, openAIModel{
			ID:      m.Name,
			Object:  "model",
			Created: openAICreated,
			OwnedBy: "agent-gpu",
		})
	}
	writeJSON(w, http.StatusOK, openAIModelList{Object: "list", Data: data})
}

// handleOpenAIModelRetrieve serves GET /v1/models/{model}, OpenAI's
// retrieve-model endpoint. It returns the single model object in the OpenAI shape
// IFF the model is visible to the key (present on an Online worker AND the key may
// run inference against it), reusing the same per-key catalog as the list
// endpoint so visibility is identical. A model that is unknown or hidden from the
// key returns 404 model_not_found — the same response for "does not exist" and
// "not permitted to see", so the endpoint never reveals the existence of a model
// the key cannot use. The {model...} wildcard captures namespaced/colon tags
// whole. The method is pinned by the route pattern (GET), so the mux returns 405
// with an Allow header for other verbs before this handler runs.
func (s *Server) handleOpenAIModelRetrieve(w http.ResponseWriter, r *http.Request) {
	key, ok := keyFromContext(r.Context())
	if !ok {
		// Unreachable behind authMiddleware; defended so a future misroute fails
		// closed rather than confirming a model's existence to an unauthenticated
		// caller.
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing api key")
		return
	}
	name := r.PathValue("model")
	for _, m := range s.catalog(r.Context(), key) {
		if m.Name == name {
			writeJSON(w, http.StatusOK, openAIModel{
				ID:      m.Name,
				Object:  "model",
				Created: openAICreated,
				OwnedBy: "agent-gpu",
			})
			return
		}
	}
	writeError(w, http.StatusNotFound, "model_not_found", "model not found")
}

// ---- richer internal /models ----

type modelList struct {
	Models []modelEntry `json:"models"`
}

type modelEntry struct {
	Name        string   `json:"name"`
	Digest      string   `json:"digest"`
	WorkerCount int      `json:"worker_count"`
	Workers     []string `json:"workers"`
}

// handleModels serves GET /models, the richer internal catalog: per model the
// digest and the Online workers currently serving it (count + ids). Same
// per-key permission filter and determinism as /v1/models.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		// Advertise the permitted method on the 405 (RFC 7231 §6.5.5).
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	key, ok := keyFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing api key")
		return
	}
	models := s.catalog(r.Context(), key)
	entries := make([]modelEntry, 0, len(models))
	for _, m := range models {
		entries = append(entries, modelEntry{
			Name:        m.Name,
			Digest:      m.Digest,
			WorkerCount: len(m.Workers),
			Workers:     m.Workers,
		})
	}
	writeJSON(w, http.StatusOK, modelList{Models: entries})
}
