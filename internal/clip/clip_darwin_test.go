//go:build darwin

package clip

import "testing"

func TestMatchImageFlavor(t *testing.T) {
	tests := []struct {
		name     string
		info     string
		wantOK   bool
		wantFlav string
	}{
		{
			name:     "real PNG screenshot flavor list",
			info:     "«class PNGf», «class furl»",
			wantOK:   true,
			wantFlav: "«class PNGf»",
		},
		{
			name:     "modern JPEG 2000 class form",
			info:     "«class jp2 », «class TIFF»",
			wantOK:   true,
			wantFlav: "«class jp2 »",
		},
		{
			name:   "GIF as substring of unrelated token does not match",
			info:   "«class NSGIFData», public.my-GIF-thing",
			wantOK: false,
		},
		{
			name:   "text-only list does not match",
			info:   "«class utf8», «class ut16», «class furl»",
			wantOK: false,
		},
		{
			name:   "empty list does not match",
			info:   "",
			wantOK: false,
		},
		{
			name:   "whitespace-only list does not match",
			info:   "   ",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flav, ok := matchImageFlavor(tt.info)
			if ok != tt.wantOK {
				t.Fatalf("matchImageFlavor(%q) ok = %v, want %v", tt.info, ok, tt.wantOK)
			}
			if ok && flav != tt.wantFlav {
				t.Fatalf("matchImageFlavor(%q) flavor = %q, want %q", tt.info, flav, tt.wantFlav)
			}
		})
	}
}

func TestMatchConcealedType(t *testing.T) {
	tests := []struct {
		name  string
		types string
		want  bool
	}{
		{"empty", "", false},
		{"plain text only", "public.utf8-plain-text\nNSStringPboardType", false},
		{"concealed present", "public.utf8-plain-text\norg.nspasteboard.ConcealedType", true},
		{"transient present", "org.nspasteboard.TransientType\npublic.utf8-plain-text", true},
		{"concealed with whitespace", "  org.nspasteboard.ConcealedType  \n", true},
		{"substring not a match", "com.example.org.nspasteboard.ConcealedTypeX", false},
		{"blank lines tolerated", "\n\norg.nspasteboard.ConcealedType\n\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchConcealedType(tt.types); got != tt.want {
				t.Errorf("matchConcealedType(%q) = %v, want %v", tt.types, got, tt.want)
			}
		})
	}
}
