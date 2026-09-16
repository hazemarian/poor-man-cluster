package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/config"
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Print recent JSON logs written by `pmcluster serve` and CLI commands",
	Long: `Reads ~/.pmcluster/logs/pmcluster-YYYY-MM-DD.log. Output is raw JSON
so it pipes into jq:
  pmcluster logs --tail=200 | jq 'select(.level=="error")'`,
	RunE: runLogs,
}

func init() {
	logsCmd.Flags().Int("tail", 100, "show the last N lines (0 = all)")
	logsCmd.Flags().Duration("since", 0, "only show entries newer than this duration ago (e.g. 24h)")
	logsCmd.Flags().Bool("follow", false, "stream new entries as they're written (Ctrl-C to stop)")
	logsCmd.Flags().Bool("all-files", false, "merge across all daily files instead of just the most recent")

	rootCmd.AddCommand(logsCmd)
}

func runLogs(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	tail, _ := cmd.Flags().GetInt("tail")
	since, _ := cmd.Flags().GetDuration("since")
	follow, _ := cmd.Flags().GetBool("follow")
	allFiles, _ := cmd.Flags().GetBool("all-files")

	files, err := dailyLogFiles(cfg.LogsDir())
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no log files yet — start `pmcluster serve` or run a command that logs)")
		return nil
	}
	if !allFiles {
		files = files[len(files)-1:]
	}

	var sinceCutoff time.Time
	if since > 0 {
		sinceCutoff = time.Now().Add(-since)
	}

	var lines []string
	for _, f := range files {
		fileLines, err := readLogLines(f, sinceCutoff)
		if err != nil {
			return err
		}
		lines = append(lines, fileLines...)
	}

	if tail > 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}

	out := cmd.OutOrStdout()
	for _, line := range lines {
		fmt.Fprintln(out, line)
	}

	if follow {
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return followLogs(ctx, cfg.LogsDir(), out)
	}
	return nil
}

// dailyLogFiles returns logs sorted ascending by date.
func dailyLogFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read logs dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(name, "pmcluster-") && strings.HasSuffix(name, ".log") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	sort.Strings(files)
	return files, nil
}

// readLogLines filters to entries newer than sinceCutoff (zero = no filter).
func readLogLines(path string, sinceCutoff time.Time) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var out []string
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64*1024), 1024*1024)
	for scan.Scan() {
		line := scan.Text()
		if !sinceCutoff.IsZero() {
			if !lineNewerThan(line, sinceCutoff) {
				continue
			}
		}
		out = append(out, line)
	}
	if err := scan.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return out, nil
}

// lineNewerThan keeps malformed/no-timestamp lines on the side of
// over-showing rather than silently dropping.
func lineNewerThan(line string, cutoff time.Time) bool {
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return true
	}
	tsRaw, ok := entry["time"]
	if !ok {
		return true
	}
	tsStr, ok := tsRaw.(string)
	if !ok {
		return true
	}
	t, err := time.Parse(time.RFC3339Nano, tsStr)
	if err != nil {
		return true
	}
	return !t.Before(cutoff)
}

// followLogs re-evaluates the latest file every tick so midnight rotation
// is picked up automatically.
func followLogs(ctx context.Context, dir string, out io.Writer) error {
	var (
		currentPath string
		f           *os.File
	)
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	for {
		files, err := dailyLogFiles(dir)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-tick.C:
				continue
			}
		}
		latest := files[len(files)-1]
		if latest != currentPath {
			if f != nil {
				_ = f.Close()
				f = nil
			}
			nf, err := os.Open(latest)
			if err != nil {
				return fmt.Errorf("open %s: %w", latest, err)
			}

			if _, err := nf.Seek(0, io.SeekEnd); err != nil {
				_ = nf.Close()
				return fmt.Errorf("seek %s: %w", latest, err)
			}
			f = nf
			currentPath = latest
		}

		buf := make([]byte, 16*1024)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				if _, werr := out.Write(buf[:n]); werr != nil {
					return werr
				}
			}
			if errors.Is(err, io.EOF) || err == nil && n < len(buf) {
				break
			}
			if err != nil {
				return err
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
