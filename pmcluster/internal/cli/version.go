package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/buildinfo"
)

// versionString returns the resolved version stamp for the --version flag.
func versionString() string {
	v, _, _ := buildinfo.Resolve()
	return v
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print pmcluster version information",
	RunE: func(cmd *cobra.Command, _ []string) error {
		v, c, d := buildinfo.Resolve()
		fmt.Fprintf(cmd.OutOrStdout(),
			"pmcluster %s\n  commit: %s\n  built:  %s\n  go:     %s %s/%s\n",
			v, c, d, runtime.Version(), runtime.GOOS, runtime.GOARCH,
		)
		return nil
	},
}
