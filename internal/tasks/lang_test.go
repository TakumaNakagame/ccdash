package tasks

import (
	"os"
	"testing"

	"github.com/TakumaNakagame/ccdash/internal/i18n"
)

// The tests read ccdash's Japanese text; pin it whatever $LANG says.
func TestMain(m *testing.M) {
	i18n.Set(i18n.JA)
	os.Exit(m.Run())
}
