//go:build harness

package harness

import (
	"testing"
	"time"
)

func TestZZDebug(t *testing.T) {
	s := newScratch(t)
	_ = s.admin(t)
	observer := s.admin(t)
	dir := copyMigrations(t)
	chain, next := chainPlus(t, dir)
	_ = chain
	slow := ver(next)
	writePair(t, dir, slow, "slow", "SELECT pg_sleep(6);\n"+recordSQL(slow), removeSQL(slow))
	t0 := time.Now()
	first := startMigrator(t, migEnv(s.DSN, dir, nil), "up")
	last := ""
	for time.Since(t0) < 14*time.Second {
		rows, err := observer.Query(testCtx(t), `SELECT datname, application_name, state, left(replace(query, E'\n', ' '), 50) FROM pg_stat_activity WHERE backend_type='client backend' AND pid <> pg_backend_pid() ORDER BY pid`)
		if err != nil {
			t.Fatal(err)
		}
		cur := ""
		for rows.Next() {
			var d, a, st, q string
			rows.Scan(&d, &a, &st, &q)
			cur += "[" + d + "|" + a + "|" + st + "|" + q + "] "
		}
		rows.Close()
		if cur != last {
			t.Logf("t=%.1fs %s", time.Since(t0).Seconds(), cur)
			last = cur
		}
		time.Sleep(150 * time.Millisecond)
	}
	res := first.wait(t)
	t.Logf("migrator exit=%d elapsed=%s stdout=%q", res.Code, res.Elapsed, res.Stdout)
}
