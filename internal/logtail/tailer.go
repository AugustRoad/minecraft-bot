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

const maxLineBytes = 64 * 1024

// TailFile continuously tails path, emitting each complete line to onLine.
// It gracefully handles files that do not yet exist, truncation, line-length caps,
// and Minecraft log rotation (draining remaining bytes from the rotated FD before switching).
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
		droppingLine bool
	)

	closeCurrent := func() {
		if file != nil {
			_ = file.Close()
			file = nil
			fileInfo = nil
			reader = nil
			offset = 0
			partial.Reset()
			droppingLine = false
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
		droppingLine = false
		logger.Info("attached to minecraft log", "path", path, "offset", offset)
		return true
	}

	drainLines := func() bool {
		if file == nil {
			return false
		}
		for {
			chunk, err := reader.ReadString('\n')
			if len(chunk) > 0 {
				offset += int64(len(chunk))
				hasNewline := strings.HasSuffix(chunk, "\n")

				if !droppingLine {
					if partial.Len()+len(chunk) > maxLineBytes {
						droppingLine = true
						partial.Reset()
					} else {
						partial.WriteString(chunk)
					}
				}

				if hasNewline {
					if !droppingLine {
						full := strings.TrimRight(partial.String(), "\r\n")
						partial.Reset()
						onLine(full)
					} else {
						droppingLine = false
					}
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					return true
				}
				logger.Warn("error reading minecraft log; reopening", "err", err)
				closeCurrent()
				return false
			}
		}
	}

	// If latest.log already exists at agent startup, start from EOF to avoid replaying old logs.
	// If it does not exist yet (e.g., MC server is still booting), wait and read from byte 0 when created.
	openFile(true)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		if file == nil {
			if openFile(false) {
				drainLines()
			}
		} else {
			drainLines()

			// Detect log rotation or truncation
			if file != nil {
				stat, err := os.Stat(path)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						// Drain any final lines written to the unlinked FD before closing.
						drainLines()
						closeCurrent()
					}
				} else if !os.SameFile(fileInfo, stat) {
					logger.Info("minecraft log rotated; draining and switching to new file", "path", path)
					drainLines()
					closeCurrent()
					if openFile(false) {
						drainLines()
					}
				} else if stat.Size() < offset {
					logger.Info("minecraft log truncated; rewinding to start", "path", path)
					if _, seekErr := file.Seek(0, io.SeekStart); seekErr == nil {
						offset = 0
						reader.Reset(file)
						partial.Reset()
						droppingLine = false
						drainLines()
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
