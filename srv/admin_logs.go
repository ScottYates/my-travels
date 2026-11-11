package srv

import (
	"strconv"
	"strings"
)

// logAttr is one key=value pair from a log line. Raw is the value exactly
// as written in the file (still quoted, if it was quoted).
type logAttr struct {
	Key   string
	Value string
	Raw   string
}

// parseSlogAttrs splits a line written by slog's TextHandler into its
// key=value pairs, in order. Values containing spaces are quoted by the
// handler and may contain escaped quotes, so splitting on spaces is not
// enough. ok is false if the line is not in that format.
func parseSlogAttrs(line string) (attrs []logAttr, ok bool) {
	i := 0
	for {
		for i < len(line) && line[i] == ' ' {
			i++
		}
		if i >= len(line) {
			break
		}

		eq := strings.IndexByte(line[i:], '=')
		if eq <= 0 {
			return nil, false
		}
		key := line[i : i+eq]
		if strings.ContainsAny(key, " \"") {
			return nil, false
		}
		i += eq + 1

		start := i
		var value string
		if i < len(line) && line[i] == '"' {
			// Quoted value: runs to the next unescaped quote.
			i++
			for i < len(line) && line[i] != '"' {
				if line[i] == '\\' {
					i++
				}
				i++
			}
			if i >= len(line) {
				return nil, false
			}
			i++
			unquoted, err := strconv.Unquote(line[start:i])
			if err != nil {
				return nil, false
			}
			value = unquoted
		} else {
			for i < len(line) && line[i] != ' ' {
				i++
			}
			value = line[start:i]
		}
		attrs = append(attrs, logAttr{Key: key, Value: value, Raw: line[start:i]})
	}
	return attrs, len(attrs) > 0
}

// parseSlogLine turns one log line into a logEntry. Time, level and msg
// are pulled out; every other attribute goes into Detail as "key=value"
// pairs separated by single spaces. A line that is not in slog's text
// format is returned whole as the message.
func parseSlogLine(line string) logEntry {
	entry, _ := parseSlogEntry(line)
	return entry
}

// parseSlogEntry is parseSlogLine that also returns the attributes by key.
func parseSlogEntry(line string) (logEntry, map[string]string) {
	entry := logEntry{Raw: line}
	attrs, ok := parseSlogAttrs(line)
	if !ok {
		entry.Msg = strings.TrimSpace(line)
		return entry, nil
	}

	byKey := make(map[string]string, len(attrs))
	var detail []string
	for _, a := range attrs {
		if _, seen := byKey[a.Key]; !seen {
			byKey[a.Key] = a.Value
		}
		switch {
		case a.Key == "time" && entry.Time == "":
			entry.Time = a.Value
		case a.Key == "level" && entry.Level == "":
			entry.Level = a.Value
		case a.Key == "msg" && entry.Msg == "":
			entry.Msg = a.Value
		default:
			detail = append(detail, a.Key+"="+a.Raw)
		}
	}
	entry.Detail = strings.Join(detail, " ")
	return entry, byKey
}

// isUploadRequestPath reports whether path is one of the endpoints that
// receive a photo, video or trip archive.
func isUploadRequestPath(path string) bool {
	switch {
	case path == "/api/trips/import":
		return true
	case strings.HasPrefix(path, "/api/trips/"):
		return strings.HasSuffix(path, "/photos") || strings.HasSuffix(path, "/uploads/init")
	case strings.HasPrefix(path, "/api/uploads/"):
		return strings.HasSuffix(path, "/chunk") || strings.HasSuffix(path, "/complete")
	}
	return false
}

// isUploadIssue reports whether a parsed log line is a warning or error
// about receiving an upload. Two kinds of line qualify:
//   - messages logged by the upload, chunked-upload and import code
//   - failed (4xx/5xx) POST requests to an upload endpoint
//
// Matching on the word "upload" anywhere in the line is not used: it also
// catches every failed GET of a file under /uploads/ and requests to the
// admin pages themselves.
func isUploadIssue(e logEntry, attrs map[string]string) bool {
	if e.Level != "WARN" && e.Level != "ERROR" {
		return false
	}
	if e.Msg == "http request" {
		return attrs["method"] == "POST" && isUploadRequestPath(attrs["path"])
	}
	for _, prefix := range []string{"upload:", "chunked ", "import:"} {
		if strings.HasPrefix(e.Msg, prefix) {
			return true
		}
	}
	return false
}
