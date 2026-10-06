package audit

import (
	"encoding/json"
	"errors"
	"os"
)

// head is the anchor stored beside the log: the sequence number and hash of
// the newest entry. A hash chain alone cannot show that entries were cut off
// the end (what remains is still a valid chain); comparing against the head
// can. It is replaced on every append — it is not an audit entry and holds
// nothing that is not also in the log.
type head struct {
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
}

func headPath(logPath string) string { return logPath + ".head" }

func writeHead(logPath string, h head) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	tmp := headPath(logPath) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, headPath(logPath))
}

func readHead(logPath string) (head, bool, error) {
	var h head
	b, err := os.ReadFile(headPath(logPath))
	if errors.Is(err, os.ErrNotExist) {
		return h, false, nil
	}
	if err != nil {
		return h, false, err
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return h, false, err
	}
	return h, true, nil
}
