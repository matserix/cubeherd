package agent

import (
	"net/http"
	"strconv"
	"time"
)

func (a *Agent) metricsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/metrics/status", a.metricsStatus)
	mux.HandleFunc("GET /v1/metrics/latest", a.metricsLatest)
	mux.HandleFunc("GET /v1/metrics/node", a.nodeHistory)
	mux.HandleFunc("GET /v1/metrics/servers/{id}", a.serverHistory)
	mux.HandleFunc("DELETE /v1/metrics", a.purgeMetrics)
	mux.HandleFunc("DELETE /v1/metrics/{scope}", a.purgeMetrics)
}

func (a *Agent) metricsStatus(w http.ResponseWriter, r *http.Request) {
	st, err := a.B.MetricsStatus(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *Agent) metricsLatest(w http.ResponseWriter, r *http.Request) {
	m, err := a.B.MetricsLatest(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	if m == nil {
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// historyRange reads from, to (UTC seconds) and points; a range over two years or an empty one is refused.
func historyRange(w http.ResponseWriter, r *http.Request) (from, to time.Time, points int, ok bool) {
	q := r.URL.Query()
	f, err1 := strconv.ParseInt(q.Get("from"), 10, 64)
	t, err2 := strconv.ParseInt(q.Get("to"), 10, 64)
	if err1 != nil || err2 != nil || t <= f || t-f > 2*366*24*3600 {
		writeErr(w, http.StatusBadRequest, "", "from and to must be UTC seconds, to after from, at most two years apart")
		return
	}
	points, _ = strconv.Atoi(q.Get("points"))
	if points < 10 || points > 2000 {
		points = 300
	}
	return time.Unix(f, 0), time.Unix(t, 0), points, true
}

func (a *Agent) nodeHistory(w http.ResponseWriter, r *http.Request) {
	from, to, points, ok := historyRange(w, r)
	if !ok {
		return
	}
	h, err := a.B.NodeHistory(r.Context(), from, to, points)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (a *Agent) serverHistory(w http.ResponseWriter, r *http.Request) {
	from, to, points, ok := historyRange(w, r)
	if !ok {
		return
	}
	h, err := a.B.ServerHistory(r.Context(), r.PathValue("id"), from, to, points)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

// purgeMetrics deletes the history of "node" or one server, or of everything when no scope is named.
func (a *Agent) purgeMetrics(w http.ResponseWriter, r *http.Request) {
	if err := a.B.PurgeMetrics(r.Context(), r.PathValue("scope")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
