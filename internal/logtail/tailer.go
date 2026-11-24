package logtail

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

// TailFile continuously tails path, emitting each complete line to onLine.
// It gracefully handles files that do not yet exist, truncation, and Minecraft log rotation
// (when latest.log is archived and recreated on server restart).
func TailFile(ctx context.Context, path string, pollInterval time.Duration, logger *slog.Logger, onLine func(string)) error {
	if pollInterval <= 0 {
		pollInterval = 250 * time.Millisecond
	}
	if logger == nil {
		logger = slog.Default()
	}

	var (
		file         *os.File
		fileInfo     os.FileInfo
		reader       *bufio.Reader
		offset       int64
		partial      strings.Builder
		initialCheck = true
	)

	closeCurrent := func() {
		if file != nil {
			_ = file.Close()
			file = nil
			fileInfo = nil
			reader = nil
			offset = 0
			partial.Reset()
		}
	}
	defer closeCurrent()

	openFile := func(seekEnd bool) bool {
		f, err := os.Open(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logger.Warn("unable to open minecraft log", "path", path, "err", err)
			}
			return false
		}
		fi, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return false
		}
		if seekEnd {
			pos, err := f.Seek(0, io.SeekEnd)
			if err == nil {
				offset = pos
			}
		} else {
			offset = 0
		}
		file = f
		fileInfo = fi
		reader = bufio.NewReader(f)
		partial.Reset()
		logger.Info("attached to minecraft log", "path", path, "offset", offset)
		return true
	}

	// If latest.log already exists at agent startup, start from EOF to avoid replaying old logs.
	// If it does not exist yet (e.g., MC server is still booting), wait and read from byte 0 when created.
	if openFile(true) {
		initialCheck = false
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		if file == nil {
			if openFile(false) {
				initialCheck = false
			}
		} else {
			for {
				line, err := reader.ReadString('\n')
				if len(line) > 0 {
					offset += int64(len(line))
					if strings.HasSuffix(line, "\n") {
						full := partial.String() + strings.TrimRight(line, "\r\n")
						partial.Reset()
						onLine(full)
					} else {
						partial.WriteString(line)
					}
				}
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					logger.Warn("error reading minecraft log; reopening", "err", err)
					closeCurrent()
					break
				}
			}

			// Detect log rotation or truncation
			if file != nil {
				stat, err := os.Stat(path)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						closeCurrent()
					}
				} else if !os.SameFile(fileInfo, stat) {
					logger.Info("minecraft log rotated; switching to new file", "path", path)
					closeCurrent()
					openFile(false)
				} else if stat.Size() < offset {
					logger.Info("minecraft log truncated; rewinding to start", "path", path)
					if _, seekErr := file.Seek(0, io.SeekStart); seekErr == nil {
						offset = 0
						reader.Reset(file)
						partial.Reset()
					}
				}
			}
		}

		if initialCheck {
			initialCheck = false
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
