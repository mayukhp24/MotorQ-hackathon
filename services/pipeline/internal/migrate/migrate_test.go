package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	out, err := Render("a ${X} b ${Y}", map[string]string{"X": "1", "Y": "two"})
	if err != nil || out != "a 1 b two" {
		t.Fatalf("got %q %v", out, err)
	}
	if _, err := Render("${MISSING}", nil); err == nil {
		t.Fatal("expected error for undefined variable")
	}
}

func TestSplitStatements(t *testing.T) {
	sql := `-- header comment
CREATE TABLE a (x int);

-- another
CREATE TABLE b (
  y int -- inline; not a terminator
) SETTINGS z = 'a--b';   -- trailing comment

SELECT 1`
	got := SplitStatements(sql)
	if len(got) != 3 {
		t.Fatalf("got %d: %q", len(got), got)
	}
	if !strings.HasPrefix(got[1], "CREATE TABLE b") || strings.HasSuffix(got[1], ";") || !strings.Contains(got[1], "'a--b'") {
		t.Fatalf("bad statement %q", got[1])
	}
}

func TestQuoteLiteral(t *testing.T) {
	if quoteLiteral("it's") != "'it''s'" {
		t.Fatal(quoteLiteral("it's"))
	}
}

// The shipped ClickHouse DDL must render with the variables compose provides.
func TestShippedClickHouseDDLRenders(t *testing.T) {
	files, _ := filepath.Glob("../../../../db/clickhouse/*.sql")
	if len(files) == 0 {
		t.Skip("db/ not present")
	}
	vars := map[string]string{"KAFKA_BROKERS": "kafka:9092", "CH_KAFKA_CONSUMERS": "2", "CH_HOT_DAYS": "3",
		"CH_RETENTION_DAYS": "30", "CH_RO_PASSWORD": "x"}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		sql, err := Render(string(b), vars)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if n := len(SplitStatements(sql)); n < 10 {
			t.Fatalf("%s: only %d statements", f, n)
		}
	}
}
