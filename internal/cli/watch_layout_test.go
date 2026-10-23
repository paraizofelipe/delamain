package cli

import "testing"

func TestLayoutForShouldPickTheLayoutByWidthAndHeight(t *testing.T) {
	for _, c := range []struct {
		width, height int
		want          watchLayout
	}{
		{200, 50, layoutWide},
		{140, 32, layoutWide},
		{139, 40, layoutMedium},
		{140, 31, layoutMedium},
		{100, 24, layoutMedium},
		{99, 40, layoutCompact},
		{100, 23, layoutCompact},
		{80, 27, layoutCompact}, // tmux display-popup -w 50% -h 60% on a 160×45 terminal
		{70, 18, layoutCompact},
		{69, 30, layoutMini},
		{70, 17, layoutMini},
		{40, 12, layoutMini},
		{39, 12, layoutTooSmall},
		{40, 11, layoutTooSmall},
		{0, 0, layoutTooSmall},
	} {
		if got := layoutFor(c.width, c.height); got != c.want {
			t.Errorf("layoutFor(%d, %d) = %v, want %v", c.width, c.height, got, c.want)
		}
	}
}
