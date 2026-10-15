package app_test

import (
	"os"
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/config"
)

func TestMain(m *testing.M) {
	config.SetAllowPrivateBaseURL(true)
	os.Exit(m.Run())
}
