package device

import "testing"

func TestAppleModel(t *testing.T) {
	for id, want := range map[string]string{
		"iPhone14,2":  "iPhone 13 Pro",
		"iPhone17,3":  "iPhone 16",
		"iPad16,3":    "iPad Pro 11-inch (M4)",
		"iPad16,4":    "iPad Pro 11-inch (M4)",
		"AppleTV14,1": "Apple TV 4K (3rd generation)",
		"iPhone99,1":  "",
		"":            "",
	} {
		got, ok := AppleModel(id)
		if got != want || ok != (want != "") {
			t.Errorf("AppleModel(%q) = (%q, %t), want (%q, %t)", id, got, ok, want, want != "")
		}
	}
}
