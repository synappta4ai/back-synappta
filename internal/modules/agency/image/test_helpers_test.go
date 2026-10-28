package agencyimage

import (
	"os"
	"strings"
	"testing"
)

// removeOutputFile deletes a file written under the served /outputs directory
// so a test does not leave artifacts behind.
func removeOutputFile(t *testing.T, url string) {
	t.Helper()
	path := strings.TrimPrefix(url, "/")
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove generated output %q: %v", path, err)
	}
}
