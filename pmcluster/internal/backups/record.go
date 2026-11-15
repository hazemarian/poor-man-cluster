package backups

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// RecordPath is the node-facing route a backup agent posts to when an archive has
// been shipped: "this stack is backed up, here is the artifact".
//
// Under /internal/api with the other node routes because it is exactly the same
// kind of call: a node telling the leader about work it did, over swarm mTLS, with
// no token and no published port. The record is written by the run itself, at the
// moment the last destination confirmed the object — not later, by a scan that
// only happens when someone opens the backups page.
const RecordPath = "/internal/api/backups/record"

// maxRecordBody bounds one record. The payload is a dozen fields; the cap exists
// so a corrupt or hostile node cannot make the leader allocate.
const maxRecordBody = 64 << 10

// RecordService accepts an artifact record from a node and writes the row.
type RecordService struct {
	Store *store.Store
	Log   zerolog.Logger
}

func (s *RecordService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.Store == nil {
		http.Error(w, "no store", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRecordBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var rec store.ArtifactRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		http.Error(w, "invalid record: "+err.Error(), http.StatusBadRequest)
		return
	}
	if rec.Stack == "" || rec.Archive == "" {
		http.Error(w, "stack and archive are required", http.StatusBadRequest)
		return
	}
	if err := s.Store.RecordArtifact(r.Context(), rec); err != nil {
		s.Log.Warn().Err(err).Str("stack", rec.Stack).Str("archive", rec.Archive).
			Msg("backup record: could not write the row")
		http.Error(w, "could not record", http.StatusInternalServerError)
		return
	}
	s.Log.Info().Str("stack", rec.Stack).Str("node", rec.Node).Str("archive", rec.Archive).
		Str("status", rec.Status).Int("files", rec.Files).Int64("bytes", rec.Bytes).
		Msg("backup record: archived")
	w.WriteHeader(http.StatusNoContent)
}
