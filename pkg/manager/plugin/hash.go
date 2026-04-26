package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// Hash computes sha256 over content and returns "sha256:<hex>" plus byte count.
func Hash(content io.Reader) (sha string, n int64, err error) {
	h := sha256.New()
	written, copyErr := io.Copy(h, content)
	if copyErr != nil {
		return "", written, copyErr
	}

	sum := h.Sum(nil)
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(sum)), written, nil
}
