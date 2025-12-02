package protofsm

import "testing"

// TestTrimPackagePrefix verifies that trimPackagePrefix correctly handles
// various type name formats including composite types with slices and
// multiple pointer indirections.
func TestTrimPackagePrefix(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple type no package",
			input:    "MyType",
			expected: "MyType",
		},
		{
			name:     "simple type with package",
			input:    "mypackage.MyType",
			expected: "MyType",
		},
		{
			name:     "pointer type with package",
			input:    "*mypackage.MyType",
			expected: "*MyType",
		},
		{
			name:     "double pointer with package",
			input:    "**mypackage.MyType",
			expected: "**MyType",
		},
		{
			name:     "slice type with package",
			input:    "[]mypackage.MyType",
			expected: "[]MyType",
		},
		{
			name:     "slice of pointers with package",
			input:    "[]*mypackage.MyType",
			expected: "[]*MyType",
		},
		{
			name:     "slice of double pointers with package",
			input:    "[]**mypackage.MyType",
			expected: "[]**MyType",
		},
		{
			name:     "nested package path",
			input:    "*github.com/foo/bar/pkg.Type",
			expected: "*Type",
		},
		{
			name:     "slice with nested package",
			input:    "[]*github.com/foo/bar.MyStruct",
			expected: "[]*MyStruct",
		},
		{
			name:     "pointer no package",
			input:    "*MyType",
			expected: "*MyType",
		},
		{
			name:     "slice no package",
			input:    "[]MyType",
			expected: "[]MyType",
		},
		{
			name:     "slice of pointers no package",
			input:    "[]*MyType",
			expected: "[]*MyType",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := trimPackagePrefix(tc.input)
			if result != tc.expected {
				t.Errorf("trimPackagePrefix(%q) = %q, want %q",
					tc.input, result, tc.expected)
			}
		})
	}
}
