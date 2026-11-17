package library

import (
	"os"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// TestMain has the packages the tests write say the encoder that made their
// picture, as the packager's do (librarytest.VideoEncoder): the copy rule
// holds an original whose package says none.
func TestMain(m *testing.M) {
	librarytest.VideoEncoder = "libx264"
	os.Exit(m.Run())
}
