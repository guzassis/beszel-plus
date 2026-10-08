package maintenance

import (
	"io"
	"os"
	"strings"
)

const maxInstallLogBytes = 64 * 1024

type installLogCursor struct {
	path   string
	file   *os.File
	offset int64
}

// Keep an open cursor across the command so historic failures and normal log
// rotation cannot attribute an old maintainer-script error to this installation.
func (h *Helper) installLogCursors() []installLogCursor {
	var cursors []installLogCursor
	for _, path := range []string{"/var/log/apt/term.log", "/var/log/unattended-upgrades/unattended-upgrades-dpkg.log"} {
		cursor := installLogCursor{path: h.path(path)}
		file, err := os.Open(cursor.path)
		if err != nil {
			if os.IsNotExist(err) {
				cursors = append(cursors, cursor)
			}
			continue
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			file.Close()
			continue
		}
		cursor.file, cursor.offset = file, info.Size()
		cursors = append(cursors, cursor)
	}
	return cursors
}

func closeInstallLogs(cursors []installLogCursor) {
	for _, cursor := range cursors {
		if cursor.file != nil {
			cursor.file.Close()
		}
	}
}

func installFailureSummary(cursors []installLogCursor) string {
	for _, cursor := range cursors {
		file := cursor.file
		if file == nil {
			var err error
			file, err = os.Open(cursor.path)
			if err != nil {
				continue
			}
			defer file.Close()
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() <= cursor.offset {
			continue
		}
		start := max(cursor.offset, info.Size()-maxInstallLogBytes)
		data, err := io.ReadAll(io.NewSectionReader(file, start, maxInstallLogBytes))
		if err != nil {
			continue
		}
		var packageError string
		var details []string
		for line := range strings.Lines(string(data)) {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "dpkg: error processing package ") {
				packageError = sanitize(line, 160)
			}
			if strings.HasPrefix(line, "dracut[F]:") || strings.HasPrefix(line, "update-initramfs: failed") ||
				(strings.HasPrefix(line, "realpath:") && strings.Contains(line, "/lib/modules/")) ||
				strings.Contains(line, "postinst maintainer script subprocess failed") || strings.Contains(line, "post-installation script subprocess returned error") {
				details = append(details, sanitize(line, 160))
			}
		}
		if packageError != "" {
			// Preserve the package first within the persisted 512-byte message.
			message := packageError
			for _, detail := range details {
				if len(message)+len(detail)+2 > 470 {
					break
				}
				message += "; " + detail
			}
			return message
		}
	}
	return ""
}
