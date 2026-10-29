package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tailBytes is how much of each log LastInteractive reads. Records are
// appended, so the newest are at the end.
const tailBytes = 256 << 10

// LastInteractive returns the newest record from an interactive Claude Code
// session (entrypoint "cli") in logs modified at or after since, or the zero
// time if there is none. It reads only the end of each file, so it is cheap
// enough to call every minute. gmwd's own runs are "sdk-cli" and don't count.
func LastInteractive(root string, since time.Time) (time.Time, error) {
	var last time.Time
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(since) {
			return nil
		}
		if t := lastInteractiveIn(path, info.Size()); t.After(last) {
			last = t
		}
		return nil
	})
	return last, err
}

func lastInteractiveIn(path string, size int64) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	off := max(size-tailBytes, 0)
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return time.Time{}
	}
	lines := bytes.Split(buf, []byte{'\n'})
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // starts mid-line
	}
	var last time.Time
	for _, line := range lines {
		if !bytes.Contains(line, []byte(`"entrypoint":"cli"`)) {
			continue
		}
		var rec struct {
			Timestamp  time.Time `json:"timestamp"`
			Entrypoint string    `json:"entrypoint"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.Entrypoint == "cli" && rec.Timestamp.After(last) {
			last = rec.Timestamp
		}
	}
	return last
}
