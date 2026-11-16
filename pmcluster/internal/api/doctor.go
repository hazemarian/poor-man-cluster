package api

import (
	"net/http"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/doctor"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// DoctorHandler runs the platform health checks and reports them as JSON.
//
// The daemon already holds the store, the cipher and the docker client, so this
// is the same measurement `pmcluster doctor` makes.
//
// NOTHING IN THE CONSOLE CALLS THIS YET (checked 2026-11-16: no template, no script, no pmclient
// method). An earlier comment claimed it was "reachable from the console", which was untrue and
// made the health model look visible when it is CLI-only. Wiring a Health page here is open work.
// and from anything that wants to gate on platform health.
func DoctorHandler(dc runtime.Client, st *store.Store, cipher *credentials.Cipher, gha doctor.GitHubAppProbe) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		volumeRoot := "/var/stack/data"
		if st != nil {
			volumeRoot = st.SettingDefault(r.Context(), cluster.SettingVolumeRoot(), volumeRoot)
		}
		rep, err := doctor.Run(r.Context(), doctor.Input{
			Docker:         dc,
			Store:          st,
			Cipher:         cipher,
			GitHubApp:      gha,
			Version:        buildinfo.Version,
			BackupStateDir: "/var/lib/pmcluster-backup",
			VolumeRoot:     volumeRoot,
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rep)
	}
}
