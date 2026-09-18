package linters

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// TestFrameworkAnalyzer checks the framework-specific semantic invariants.
func TestFrameworkAnalyzer(t *testing.T) {
	t.Parallel()

	analysistest.Run(
		t, analysistest.TestData(), frameworkAnalyzer, "frameworktest",
	)
}
