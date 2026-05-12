package linters

import "testing"

func TestExpandLeadingTabs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want string
	}{{
		name: "empty",
		line: "",
		want: "",
	}, {
		name: "no leading whitespace",
		line: "noindent",
		want: "noindent",
	}, {
		name: "only tab",
		line: "\t",
		want: "        ",
	}, {
		name: "mixed space and tab indent",
		line: "   \tcode",
		want: "           code",
	}, {
		name: "tabs inside string",
		line: "\t\twriter, \"A\tB\tC\"",
		want: "                writer, \"A\tB\tC\"",
	}}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := expandLeadingTabs(test.line, "        ")
			if got != test.want {
				t.Fatalf("expected %q, got %q", test.want, got)
			}
		})
	}
}
