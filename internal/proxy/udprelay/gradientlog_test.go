package udprelay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAppendGradientLogColumnsMatchHeader сторожит ровно один класс ошибки:
// столбец добавили в заголовок и забыли в строке (или наоборот). Такой лог не
// падает и не жалуется - он просто тихо разбирается по сдвинутым $N, и это
// выясняется на разборе через дни накопления. Заодно фиксирует сам порядок
// столбцов: на него завязаны рецепты в docs/hotset-autoscaler.md §4.
func TestAppendGradientLogColumnsMatchHeader(t *testing.T) {
	t.Chdir(t.TempDir()) // gradientLogFile - относительный путь, CWD ядра

	appendGradientLog(time.Now(), 30, 42*time.Millisecond, 35*time.Millisecond, 1.006, 36, 0, 118447296, 1734128640)
	appendGradientLog(time.Now(), 30, 96*time.Millisecond, 35*time.Millisecond, 0.438, 19, -1, 402653184, 6115295232)

	raw, err := os.ReadFile(filepath.Join(".", gradientLogFile))
	if err != nil {
		t.Fatalf("лог не создан: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("строк %d, ожидалось 3 (заголовок + два тика): %q", len(lines), lines)
	}

	header := strings.Split(lines[0], ",")
	want := []string{"timestamp", "k", "avg_rtt_ms", "min_rtt_ms", "gradient", "suggested_k", "delta", "tx_bytes", "rx_bytes"}
	if len(header) != len(want) {
		t.Fatalf("в заголовке %d столбцов, ожидалось %d: %q", len(header), len(want), lines[0])
	}
	for i := range want {
		if header[i] != want[i] {
			t.Errorf("столбец %d: %q, ожидался %q", i+1, header[i], want[i])
		}
	}

	for i, line := range lines[1:] {
		if got := len(strings.Split(line, ",")); got != len(want) {
			t.Errorf("тик %d: %d полей против %d в заголовке: %q", i+1, got, len(want), line)
		}
	}
	if !strings.HasSuffix(lines[2], ",-1,402653184,6115295232") {
		t.Errorf("хвост строки с действием: %q", lines[2])
	}
}
