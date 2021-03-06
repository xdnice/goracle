// Copyright 2017 Tamás Gulácsi
//
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package goracle_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"

	goracle "gopkg.in/goracle.v2"
)

var (
	testDb *sql.DB
	tl     = &testLogger{}

	clientVersion, serverVersion goracle.VersionInfo
	testConStr                   string
)

func init() {
	goracle.Log = tl.GetLog()

	testConStr = fmt.Sprintf("oracle://%s:%s@%s/?poolMinSessions=1&poolMaxSessions=16&poolIncrement=1&connectionClass=POOLED",
		os.Getenv("GORACLE_DRV_TEST_USERNAME"),
		os.Getenv("GORACLE_DRV_TEST_PASSWORD"),
		os.Getenv("GORACLE_DRV_TEST_DB"),
	)
	var err error
	if testDb, err = sql.Open("goracle", testConStr); err != nil {
		fmt.Printf("ERROR: %+v\n", err)
		return
		//panic(err)
	}

	if clientVersion, err = goracle.ClientVersion(testDb); err != nil {
		fmt.Printf("ERROR: %+v\n", err)
		return
	}
	if serverVersion, err = goracle.ServerVersion(testDb); err != nil {
		fmt.Printf("ERROR: %+v\n", err)
		return
	}
}

var bufPool = sync.Pool{New: func() interface{} { return bytes.NewBuffer(make([]byte, 0, 1024)) }}

type testLogger struct {
	sync.RWMutex
	Ts       []*testing.T
	beHelped []*testing.T
}

func (tl *testLogger) GetLog() func(keyvals ...interface{}) error {
	return func(keyvals ...interface{}) error {
		buf := bufPool.Get().(*bytes.Buffer)
		defer bufPool.Put(buf)
		buf.Reset()
		if len(keyvals)%2 != 0 {
			keyvals = append(append(make([]interface{}, 0, len(keyvals)+1), "msg"), keyvals...)
		}
		for i := 0; i < len(keyvals); i += 2 {
			fmt.Fprintf(buf, "%s=%#v ", keyvals[i], keyvals[i+1])
		}

		tl.Lock()
		for _, t := range tl.beHelped {
			t.Helper()
		}
		tl.beHelped = tl.beHelped[:0]
		tl.Unlock()

		tl.RLock()
		defer tl.RUnlock()
		for _, t := range tl.Ts {
			t.Helper()
			t.Log(buf.String())
		}

		return nil
	}
}
func (tl *testLogger) enableLogging(t *testing.T) func() {
	tl.Lock()
	tl.Ts = append(tl.Ts, t)
	tl.beHelped = append(tl.beHelped, t)
	tl.Unlock()

	return func() {
		tl.Lock()
		defer tl.Unlock()
		for i, f := range tl.Ts {
			if f == t {
				tl.Ts[i] = tl.Ts[0]
				tl.Ts = tl.Ts[1:]
				break
			}
		}
		for i, f := range tl.beHelped {
			if f == t {
				tl.beHelped[i] = tl.beHelped[0]
				tl.beHelped = tl.beHelped[1:]
				break
			}
		}
	}
}

func TestDbmsOutput(t *testing.T) {
	t.Parallel()
	defer tl.enableLogging(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := testDb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := goracle.EnableDbmsOutput(ctx, conn); err != nil {
		t.Fatal(err)
	}

	txt := `árvíztűrő tükörfúrógép`
	qry := "BEGIN DBMS_OUTPUT.PUT_LINE('" + txt + "'); END;"
	if _, err := conn.ExecContext(ctx, qry); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := goracle.ReadDbmsOutput(ctx, &buf, conn); err != nil {
		t.Error(err)
	}
	t.Log(buf.String())
	if buf.String() != txt+"\n" {
		t.Errorf("got %q, wanted %q", buf.String(), txt+"\n")
	}
}

func TestInOutArray(t *testing.T) {
	t.Parallel()
	defer tl.enableLogging(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := testDb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	qry := `CREATE OR REPLACE PACKAGE test_pkg AS
TYPE int_tab_typ IS TABLE OF BINARY_INTEGER INDEX BY PLS_INTEGER;
TYPE num_tab_typ IS TABLE OF NUMBER INDEX BY PLS_INTEGER;
TYPE vc_tab_typ IS TABLE OF VARCHAR2(100) INDEX BY PLS_INTEGER;
TYPE dt_tab_typ IS TABLE OF DATE INDEX BY PLS_INTEGER;
TYPE lob_tab_typ IS TABLE OF CLOB INDEX BY PLS_INTEGER;

PROCEDURE inout_int(p_int IN OUT int_tab_typ);
PROCEDURE inout_num(p_num IN OUT num_tab_typ);
PROCEDURE inout_vc(p_vc IN OUT vc_tab_typ);
PROCEDURE inout_dt(p_dt IN OUT dt_tab_typ);
PROCEDURE p2(
	--p_int IN OUT int_tab_typ,
	p_num IN OUT num_tab_typ, p_vc IN OUT vc_tab_typ, p_dt IN OUT dt_tab_typ);
END test_pkg;
`
	if _, err := conn.ExecContext(ctx, qry); err != nil {
		t.Fatal(err, qry)
	}
	defer testDb.Exec("DROP PACKAGE test_pkg")

	qry = `CREATE OR REPLACE PACKAGE BODY test_pkg AS
PROCEDURE inout_int(p_int IN OUT int_tab_typ) IS
  v_idx PLS_INTEGER;
BEGIN
  DBMS_OUTPUT.PUT_LINE('p_int.COUNT='||p_int.COUNT||' FIRST='||p_int.FIRST||' LAST='||p_int.LAST);
  v_idx := p_int.FIRST;
  WHILE v_idx IS NOT NULL LOOP
    p_int(v_idx) := NVL(p_int(v_idx) * 2, 1);
	v_idx := p_int.NEXT(v_idx);
  END LOOP;
  p_int(NVL(p_int.LAST, 0)+1) := p_int.COUNT;
END;

PROCEDURE inout_num(p_num IN OUT num_tab_typ) IS
  v_idx PLS_INTEGER;
BEGIN
  DBMS_OUTPUT.PUT_LINE('p_num.COUNT='||p_num.COUNT||' FIRST='||p_num.FIRST||' LAST='||p_num.LAST);
  v_idx := p_num.FIRST;
  WHILE v_idx IS NOT NULL LOOP
    p_num(v_idx) := NVL(p_num(v_idx) / 2, 0.5);
	v_idx := p_num.NEXT(v_idx);
  END LOOP;
  p_num(NVL(p_num.LAST, 0)+1) := p_num.COUNT;
END;

PROCEDURE inout_vc(p_vc IN OUT vc_tab_typ) IS
  v_idx PLS_INTEGER;
BEGIN
  DBMS_OUTPUT.PUT_LINE('p_vc.COUNT='||p_vc.COUNT||' FIRST='||p_vc.FIRST||' LAST='||p_vc.LAST);
  v_idx := p_vc.FIRST;
  WHILE v_idx IS NOT NULL LOOP
    p_vc(v_idx) := NVL(p_vc(v_idx) ||' +', '-');
	v_idx := p_vc.NEXT(v_idx);
  END LOOP;
  p_vc(NVL(p_vc.LAST, 0)+1) := p_vc.COUNT;
END;

PROCEDURE inout_dt(p_dt IN OUT dt_tab_typ) IS
  v_idx PLS_INTEGER;
BEGIN
  DBMS_OUTPUT.PUT_LINE('p_dt.COUNT='||p_dt.COUNT||' FIRST='||p_dt.FIRST||' LAST='||p_dt.LAST);
  v_idx := p_dt.FIRST;
  WHILE v_idx IS NOT NULL LOOP
  DBMS_OUTPUT.PUT_LINE(v_idx||'='||TO_CHAR(p_dt(v_idx), 'YYYY-MM-DD HH24:MI:SS'));
    p_dt(v_idx) := NVL(p_dt(v_idx) + 1, TRUNC(SYSDATE)-v_idx);
	v_idx := p_dt.NEXT(v_idx);
  END LOOP;
  p_dt(NVL(p_dt.LAST, 0)+1) := TRUNC(SYSDATE);
END;

PROCEDURE p2(
	--p_int IN OUT int_tab_typ,
	p_num IN OUT num_tab_typ,
	p_vc IN OUT vc_tab_typ,
	p_dt IN OUT dt_tab_typ
--, p_lob IN OUT lob_tab_typ
) IS
BEGIN
  --inout_int(p_int);
  inout_num(p_num);
  inout_vc(p_vc);
  inout_dt(p_dt);
  --p_lob := NULL;
END p2;
END test_pkg;
`
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(err, qry)
	}
	compileErrors, err := goracle.GetCompileErrors(testDb, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(compileErrors) != 0 {
		t.Logf("compile errors: %v", compileErrors)
		for _, ce := range compileErrors {
			if strings.Contains(ce.Error(), "TEST_PKG") {
				t.Fatal(ce)
			}
		}
	}

	intgr := []int32{3, 1, 4}
	intgrWant := []int32{3 * 2, 1 * 2, 4 * 2, 3}
	_ = intgrWant
	num := []goracle.Number{"3.14", "-2.48"}
	numWant := []goracle.Number{"1.57", "-1.24", "2"}
	vc := []string{"string", "bring"}
	vcWant := []string{"string +", "bring +", "2"}
	dt := []time.Time{time.Date(2017, 6, 18, 7, 5, 51, 0, time.Local), time.Time{}}
	today := time.Now().Truncate(24 * time.Hour)
	today = time.Date(today.Year(), today.Month(), today.Day(), today.Hour(), today.Minute(), today.Second(), 0, time.Local)
	dtWant := []time.Time{
		dt[0].Add(24 * time.Hour),
		today.Add(-2 * 24 * time.Hour),
		today,
	}

	goracle.EnableDbmsOutput(ctx, testDb)

	opts := []cmp.Option{
		cmp.Comparer(func(x, y time.Time) bool {
			d := x.Sub(y)
			if d < 0 {
				d *= -1
			}
			return d <= 2*time.Hour
		}),
	}

	for _, tC := range []struct {
		Name     string
		In, Want interface{}
	}{
		{Name: "vc", In: vc, Want: vcWant},
		{Name: "num", In: num, Want: numWant},
		{Name: "dt", In: dt, Want: dtWant},
		//{Name: "int", In: intgr, Want: intgrWant},
	} {
		in := copySlice(tC.In)
		t.Logf("%s=%s", tC.Name, in)
		qry = "BEGIN test_pkg.inout_" + tC.Name + "(:1); END;"
		if _, err := testDb.ExecContext(ctx, qry,
			goracle.PlSQLArrays,
			sql.Out{Dest: &(in), In: true},
		); err != nil {
			t.Fatalf("%s\n%+v", qry, err)
		}

		if cmp.Equal(in, tC.Want, opts...) {
			continue
		}
		t.Errorf("%s: %s", tC.Name, cmp.Diff(in, tC.Want))
		var buf bytes.Buffer
		if err := goracle.ReadDbmsOutput(ctx, &buf, testDb); err != nil {
			t.Error(err)
		}
		t.Log("OUTPUT:", buf.String())
		return
	}

	//lob := []goracle.Lob{goracle.Lob{IsClob: true, Reader: strings.NewReader("abcdef")}}
	if _, err := testDb.ExecContext(ctx,
		"BEGIN test_pkg.p2(:1, :2, :3); END;",
		goracle.PlSQLArrays,
		//sql.Out{Dest: &intgr, In: true},
		sql.Out{Dest: &num, In: true},
		sql.Out{Dest: &vc, In: true},
		sql.Out{Dest: &dt, In: true},
		//sql.Out{Dest: &lob, In: true},
	); err != nil {
		t.Fatal(err)
	}
	t.Logf("int=%#v num=%#v vc=%#v dt=%#v", intgr, num, vc, dt)
	//if d := cmp.Diff(intgr, intgrWant); d != "" {
	//	t.Errorf("int: %s", d)
	//}
	if d := cmp.Diff(num, numWant); d != "" {
		t.Errorf("num: %s", d)
	}
	if d := cmp.Diff(vc, vcWant); d != "" {
		t.Errorf("vc: %s", d)
	}
	if !cmp.Equal(dt, dtWant, opts...) {
		if d := cmp.Diff(dt, dtWant); d != "" {
			t.Errorf("dt: %s", d)
		}
	}
}

func TestOutParam(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := testDb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	qry := `CREATE OR REPLACE PROCEDURE
test_p1(p_int IN OUT INTEGER, p_num IN OUT NUMBER, p_vc IN OUT VARCHAR2, p_dt IN OUT DATE, p_lob IN OUT CLOB)
IS
BEGIN
  p_int := NVL(p_int * 2, 1);
  p_num := NVL(p_num / 2, 0.5);
  p_vc := NVL(p_vc ||' +', '-');
  p_dt := NVL(p_dt + 1, SYSDATE);
  p_lob := NULL;
END;`
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(err, qry)
	}
	defer testDb.Exec("DROP PROCEDURE test_p1")

	qry = "BEGIN test_p1(:1, :2, :3, :4, :5); END;"
	stmt, err := testDb.PrepareContext(ctx, qry)
	if err != nil {
		t.Fatal(errors.Wrap(err, qry))
	}
	defer stmt.Close()

	var intgr int = 3
	num := goracle.Number("3.14")
	var vc string = "string"
	var dt time.Time = time.Date(2017, 6, 18, 7, 5, 51, 0, time.Local)
	var lob goracle.Lob = goracle.Lob{IsClob: true, Reader: strings.NewReader("abcdef")}
	if _, err := stmt.ExecContext(ctx,
		sql.Out{Dest: &intgr, In: true},
		sql.Out{Dest: &num, In: true},
		sql.Out{Dest: &vc, In: true},
		sql.Out{Dest: &dt, In: true},
		sql.Out{Dest: &lob, In: true},
	); err != nil {
		t.Fatal(errors.Wrap(err, qry))
	}
	t.Logf("int=%#v num=%#v vc=%#v dt=%#v", intgr, num, vc, dt)
	if intgr != 6 {
		t.Errorf("int: got %d, wanted %d", intgr, 6)
	}
	if num != "1.57" {
		t.Errorf("num: got %q, wanted %q", num, "1.57")
	}
	if vc != "string +" {
		t.Errorf("vc: got %q, wanted %q", vc, "string +")
	}
}

func TestSelectRefCursor(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const num = 1000
	rows, err := testDb.QueryContext(ctx, "SELECT CURSOR(SELECT object_name, object_type, object_id, created FROM all_objects WHERE ROWNUM < 1000) FROM DUAL")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var intf interface{}
		if err := rows.Scan(&intf); err != nil {
			t.Error(err)
			continue
		}
		t.Logf("%T", intf)
		sub := intf.(driver.RowsColumnTypeScanType)
		cols := sub.Columns()
		t.Log("Columns", cols)
		dests := make([]driver.Value, len(cols))
		for {
			if err := sub.Next(dests); err != nil {
				if err == io.EOF {
					break
				}
				t.Error(err)
				break
			}
			//fmt.Println(dests)
			t.Log(dests)
		}
		sub.Close()
	}
}

func TestSelect(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := testDb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	const num = 1000
	rows, err := testDb.QueryContext(ctx, "SELECT object_name, object_type, object_id, created FROM all_objects WHERE ROWNUM < NVL(:alpha, 2) ORDER BY object_id", sql.Named("alpha", num))
	//rows, err := testDb.QueryContext(ctx, "SELECT object_name, object_type, object_id, created FROM all_objects WHERE ROWNUM < 1000 ORDER BY object_id")
	if err != nil {
		t.Fatalf("%+v", err)
	}
	n, oldOid := 0, int64(0)
	for rows.Next() {
		var tbl, typ string
		var oid int64
		var created time.Time
		if err := rows.Scan(&tbl, &typ, &oid, &created); err != nil {
			t.Fatal(err)
		}
		t.Log(tbl, typ, oid, created)
		if tbl == "" {
			t.Fatal("empty tbl")
		}
		n++
		if oldOid > oid {
			t.Errorf("got oid=%d, wanted sth < %d.", oid, oldOid)
		}
		oldOid = oid
	}
	if n != num-1 {
		t.Errorf("got %d rows, wanted %d", n, num-1)
	}
}

func TestExecuteMany(t *testing.T) {
	t.Parallel()
	defer tl.enableLogging(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := testDb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.ExecContext(ctx, "DROP TABLE test_em")
	conn.ExecContext(ctx, "CREATE TABLE test_em (f_id INTEGER, f_int INTEGER, f_num NUMBER, f_num_6 NUMBER(6), F_num_5_2 NUMBER(5,2), f_vc VARCHAR2(30), F_dt DATE)")
	defer testDb.Exec("DROP TABLE test_em")

	const num = 1000
	ints := make([]int, num)
	nums := make([]goracle.Number, num)
	int32s := make([]int32, num)
	floats := make([]float64, num)
	strs := make([]string, num)
	dates := make([]time.Time, num)
	now := time.Now()
	ids := make([]int, num)
	for i := range nums {
		ids[i] = i
		ints[i] = i << 1
		nums[i] = goracle.Number(strconv.Itoa(i))
		int32s[i] = int32(i)
		floats[i] = float64(i) / float64(3.14)
		strs[i] = fmt.Sprintf("%x", i)
		dates[i] = now.Add(-time.Duration(i) * time.Hour)
	}
	for i, tc := range []struct {
		Name  string
		Value interface{}
	}{
		{"f_int", ints},
		{"f_num", nums},
		{"f_num_6", int32s},
		{"f_num_5_2", floats},
		{"f_vc", strs},
		{"f_dt", dates},
	} {
		res, err := conn.ExecContext(ctx,
			"INSERT INTO test_em ("+tc.Name+") VALUES (:1)",
			tc.Value)
		if err != nil {
			t.Fatalf("%d. INSERT INTO test_em (%q) VALUES (%+v): %#v", i, tc.Name, tc.Value, err)
		}
		ra, err := res.RowsAffected()
		if err != nil {
			t.Error(err)
		} else if ra != num {
			t.Errorf("%d. %q: wanted %d rows, got %d", i, tc.Name, num, ra)
		}
	}

	conn.ExecContext(ctx, "TRUNCATE TABLE test_em")

	res, err := conn.ExecContext(ctx,
		`INSERT INTO test_em
		  (f_id, f_int, f_num, f_num_6, F_num_5_2, F_vc, F_dt)
		  VALUES
		  (:1, :2, :3, :4, :5, :6, :7)`,
		ids, ints, nums, int32s, floats, strs, dates)
	if err != nil {
		t.Fatalf("%#v", err)
	}
	ra, err := res.RowsAffected()
	if err != nil {
		t.Error(err)
	} else if ra != num {
		t.Errorf("wanted %d rows, got %d", num, ra)
	}

	rows, err := conn.QueryContext(ctx, "SELECT * FROM test_em ORDER BY F_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var id, Int int
		var num string
		var vc string
		var num6 int32
		var num52 float64
		var dt time.Time
		if err := rows.Scan(&id, &Int, &num, &num6, &num52, &vc, &dt); err != nil {
			t.Fatal(err)
		}
		if id != i {
			t.Fatalf("ID got %d, wanted %d.", id, i)
		}
		if Int != ints[i] {
			t.Errorf("%d. INT got %d, wanted %d.", i, Int, ints[i])
		}
		if num != string(nums[i]) {
			t.Errorf("%d. NUM got %q, wanted %q.", i, num, nums[i])
		}
		if num6 != int32s[i] {
			t.Errorf("%d. NUM_6 got %v, wanted %v.", i, num6, int32s[i])
		}
		rounded := float64(int64(floats[i]/0.005+0.5)) * 0.005
		if math.Abs(num52-rounded) > 0.05 {
			t.Errorf("%d. NUM_5_2 got %v, wanted %v.", i, num52, rounded)
		}
		if vc != strs[i] {
			t.Errorf("%d. VC got %q, wanted %q.", i, vc, strs[i])
		}
		if dt != dates[i].Truncate(time.Second) {
			t.Errorf("%d. got DT %v, wanted %v.", i, dt, dates[i])
		}
		i++
	}
}
func TestReadWriteLob(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := testDb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.ExecContext(ctx, "DROP TABLE test_lob")
	conn.ExecContext(ctx, "CREATE TABLE test_lob (f_id NUMBER(6), f_blob BLOB, f_clob CLOB)")
	defer testDb.Exec("DROP TABLE test_lob")

	stmt, err := conn.PrepareContext(ctx, "INSERT INTO test_lob (F_id, f_blob, F_clob) VALUES (:1, :2, :3)")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()

	for tN, tC := range []struct {
		Bytes  []byte
		String string
	}{
		{[]byte{0, 1, 2, 3, 4, 5}, "12345"},
	} {

		if _, err := stmt.Exec(tN*2, tC.Bytes, tC.String); err != nil {
			t.Errorf("%d/1. (%v, %q): %v", tN, tC.Bytes, tC.String, err)
			continue
		}
		if _, err := stmt.Exec(tN*2+1,
			goracle.Lob{Reader: bytes.NewReader(tC.Bytes)},
			goracle.Lob{Reader: strings.NewReader(tC.String), IsClob: true},
		); err != nil {
			t.Errorf("%d/2. (%v, %q): %v", tN, tC.Bytes, tC.String, err)
		}

		rows, err := conn.QueryContext(ctx, "SELECT F_id, F_blob, F_clob FROM test_lob WHERE F_id IN (:1, :2)", 2*tN, 2*tN+1)
		if err != nil {
			t.Errorf("%d/3. %v", tN, err)
			continue
		}
		for rows.Next() {
			var id, blob, clob interface{}
			if err := rows.Scan(&id, &blob, &clob); err != nil {
				rows.Close()
				t.Errorf("%d/3. scan: %v", tN, err)
				continue
			}
			t.Logf("%d. blob=%+v clob=%+v", id, blob, clob)
			if clob, ok := clob.(*goracle.Lob); !ok {
				t.Errorf("%d. %T is not LOB", id, blob)
			} else {
				got, err := ioutil.ReadAll(clob)
				if err != nil {
					t.Errorf("%d. %v", id, err)
				} else if got := string(got); got != tC.String {
					t.Errorf("%d. got %q for CLOB, wanted %q", id, got, tC.String)
				}
			}
			if blob, ok := blob.(*goracle.Lob); !ok {
				t.Errorf("%d. %T is not LOB", id, blob)
			} else {
				got, err := ioutil.ReadAll(blob)
				if err != nil {
					t.Errorf("%d. %v", id, err)
				} else if !bytes.Equal(got, tC.Bytes) {
					t.Errorf("%d. got %v for BLOB, wanted %v", id, got, tC.Bytes)
				}
			}
		}
		rows.Close()
	}
}

func copySlice(orig interface{}) interface{} {
	ro := reflect.ValueOf(orig)
	rc := reflect.MakeSlice(ro.Type(), ro.Len(), ro.Cap())
	for i := 0; i < ro.Len(); i++ {
		rc.Index(i).Set(ro.Index(i))
	}
	return rc.Interface()
}

func TestObject(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := testDb.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	qry := `CREATE OR REPLACE PACKAGE test_pkg_obj IS
  TYPE int_tab_typ IS TABLE OF PLS_INTEGER INDEX BY PLS_INTEGER;
  TYPE rec_typ IS RECORD (int PLS_INTEGER, num NUMBER, vc VARCHAR2(1000), c CHAR(1000), dt DATE);
  TYPE tab_typ IS TABLE OF rec_typ INDEX BY PLS_INTEGER;
END;`
	if _, err := conn.ExecContext(ctx, qry); err != nil {
		t.Fatal(errors.Wrap(err, qry))
	}
	defer testDb.Exec("DROP PACKAGE test_pkg_obj")

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	defer tl.enableLogging(t)()
	ot, err := goracle.GetObjectType(tx, "test_pkg_obj.int_tab_typ")
	if err != nil {
		if clientVersion.Version >= 12 && serverVersion.Version >= 12 {
			t.Fatal(fmt.Sprintf("%+v", err))
		}
		t.Log(err)
		t.Skip("client or server version < 12")
	}
	t.Log(ot)
}

func TestOpenBadMemory(t *testing.T) {
	var mem runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&mem)
	t.Log("Allocated 0:", mem.Alloc)
	zero := mem.Alloc
	for i := 0; i < 100; i++ {
		badConStr := strings.Replace(testConStr, "@", fmt.Sprintf("BAD%dBAD@", i), 1)
		db, err := sql.Open("goracle", badConStr)
		if err != nil {
			t.Fatalf("bad connection string %q didn't produce error!", badConStr)
		}
		db.Close()
		runtime.GC()
		runtime.ReadMemStats(&mem)
		t.Logf("Allocated %d: %d", i+1, mem.Alloc)
	}
	d := mem.Alloc - zero
	t.Logf("atlast: %d", d)
	if d > 1<<15 {
		t.Errorf("Consumed more than 32KiB of memory: %d", d)
	}
}

func TestSelectFloat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	qry := `CREATE TABLE test_NUMBERs (
  INT_COL     NUMBER,
  FLOAT_COL  NUMBER,
  EMPTY_INT_COL NUMBER
)`
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(errors.Wrap(err, qry))
	}
	defer testDb.Exec("DROP TABLE test_numbers")

	const INT, FLOAT = 1234567, 4.5
	qry = `INSERT INTO test_numbers
	(INT_COL, FLOAT_COL, EMPTY_INT_COL)
     VALUES (1234567, 45/10, NULL)`
	if _, err := testDb.ExecContext(ctx, qry); err != nil {
		t.Fatal(errors.Wrap(err, qry))
	}

	qry = "SELECT int_col, float_col, empty_int_col FROM test_numbers"
	type numbers struct {
		Int     int
		Int64   int64
		Float   float64
		NInt    sql.NullInt64
		String  string
		NString sql.NullString
		Number  goracle.Number
	}
	var n numbers
	var i1, i2, i3 interface{}
	for tName, tC := range map[string]struct {
		Dest [3]interface{}
		Want numbers
	}{
		"int,float,nstring": {
			Dest: [3]interface{}{&n.Int, &n.Float, &n.NString},
			Want: numbers{Int: INT, Float: FLOAT},
		},
		"inf,float,Number": {
			Dest: [3]interface{}{&n.Int, &n.Float, &n.Number},
			Want: numbers{Int: INT, Float: FLOAT},
		},
		"int64,float,nullInt": {
			Dest: [3]interface{}{&n.Int64, &n.Float, &n.NInt},
			Want: numbers{Int64: INT, Float: FLOAT},
		},
		"intf,intf,intf": {
			Dest: [3]interface{}{&i1, &i2, &i3},
			Want: numbers{Int64: INT, Float: FLOAT},
		},
		"int,float,string": {
			Dest: [3]interface{}{&n.Int, &n.Float, &n.String},
			Want: numbers{Int: INT, Float: FLOAT},
		},
	} {
		i1, i2, i3 = nil, nil, nil
		n = numbers{}
		F := func() error {
			return errors.Wrap(
				testDb.QueryRowContext(ctx, qry).Scan(tC.Dest[0], tC.Dest[1], tC.Dest[2]),
				qry)
		}
		if err := F(); err != nil {
			if strings.HasSuffix(err.Error(), "unsupported Scan, storing driver.Value type <nil> into type *string") {
				t.Log("WARNING:", err)
				continue
			}
			noLogging := tl.enableLogging(t)
			err = F()
			t.Errorf("%q: %v", tName, errors.Wrap(err, qry))
			noLogging()
			continue
		}
		if tName == "intf,intf,intf" {
			t.Logf("%q: %#v, %#v, %#v", tName, i1, i2, i3)
			continue
		}
		t.Logf("%q: %+v", tName, n)
		if n != tC.Want {
			t.Errorf("%q:\ngot\t%+v,\nwanted\t%+v.", tName, n, tC.Want)
		}
	}
}

func TestNumInputs(t *testing.T) {
	var a, b string
	if err := testDb.QueryRow("SELECT :1, :2 FROM DUAL", 'a', 'b').Scan(&a, &b); err != nil {
		t.Errorf("two inputs: %+v", err)
	}
	if err := testDb.QueryRow("SELECT :a, :b FROM DUAL", 'a', 'b').Scan(&a, &b); err != nil {
		t.Errorf("two named inputs: %+v", err)
	}
	if err := testDb.QueryRow("SELECT :a, :a FROM DUAL", sql.Named("a", a)).Scan(&a, &b); err != nil {
		t.Errorf("named inputs: %+v", err)
	}
}
