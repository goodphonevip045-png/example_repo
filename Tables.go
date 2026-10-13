package main

// ระบบโต๊ะ: โต๊ะแต่ละตัว (dining_tables), จัดโต๊ะอัตโนมัติตอนจอง, กระดานสถานะโต๊ะ
// และหน้าจัดการโต๊ะของ admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

var (
	errBookingBusy = errors.New("booking lock timeout")
	errNotFound    = errors.New("not found")
)

// noTableError: ไม่มีโต๊ะว่างที่รองรับจำนวนคนในช่วงเวลานั้น
type noTableError struct {
	TooBig   bool // จำนวนคนมากกว่าโต๊ะที่ใหญ่ที่สุด (ต่อให้ว่างหมดก็ไม่พอ)
	MaxSeats int  // จำนวนที่นั่งของโต๊ะที่ใหญ่ที่สุดที่เปิดใช้งาน (0 = ไม่มีโต๊ะเลย)
}

func (e *noTableError) Error() string { return "no table available" }

func noTableMessage(e *noTableError, size int) string {
	switch {
	case e.MaxSeats == 0:
		return "ยังไม่มีโต๊ะที่เปิดใช้งานในระบบ กรุณาติดต่อร้าน"
	case e.TooBig:
		return fmt.Sprintf("ไม่มีโต๊ะที่รองรับ %d คน (โต๊ะใหญ่สุดรองรับ %d คน) กรุณาแยกจอง หรือติดต่อร้านโดยตรง", size, e.MaxSeats)
	default:
		return fmt.Sprintf("ช่วงเวลานี้โต๊ะที่รองรับ %d คนเต็มแล้ว กรุณาเลือกเวลาอื่น หรือดูสถานะโต๊ะด้านล่างก่อนจอง", size)
	}
}

// queryer: ใช้ได้ทั้ง *sql.DB และ *sql.Conn
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// withDateLock: ล็อกระดับ "วันที่" ด้วย GET_LOCK ของ MySQL เพื่อกันสองคนแย่งโต๊ะเดียวกันพร้อมกัน
func withDateLock(ctx context.Context, date string, fn func(conn *sql.Conn) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	lockName := "booking:" + date
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 5)", lockName).Scan(&got); err != nil {
		return err
	}
	if !got.Valid || got.Int64 != 1 {
		return errBookingBusy
	}
	defer func() {
		var released sql.NullInt64
		if err := conn.QueryRowContext(context.Background(), "SELECT RELEASE_LOCK(?)", lockName).Scan(&released); err != nil {
			fmt.Printf("ERROR release lock %s: %v\n", lockName, err)
		}
	}()
	return fn(conn)
}

// withInventoryLock serializes table allocation with admin table changes.
// Callers that also need a date lock must acquire the date lock first.
func withInventoryLock(ctx context.Context, fn func(conn *sql.Conn) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	const lockName = "table-inventory"
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 5)", lockName).Scan(&got); err != nil {
		return err
	}
	if !got.Valid || got.Int64 != 1 {
		return errBookingBusy
	}
	defer func() {
		var released sql.NullInt64
		if err := conn.QueryRowContext(context.Background(), "SELECT RELEASE_LOCK(?)", lockName).Scan(&released); err != nil {
			fmt.Printf("ERROR release lock %s: %v\n", lockName, err)
		}
	}()
	return fn(conn)
}

// timesOverlap: ช่วงนั่ง [aStart, aStart+diningMin) ทับกับ [bStart, bStart+diningMin) หรือไม่
func timesOverlap(aStart, bStart int) bool {
	return aStart < bStart+diningMin && bStart < aStart+diningMin
}

// busyTables: โต๊ะ (table_id) ที่ถูกจองทับช่วงเวลาที่ขอ (ไม่นับรายการที่ยกเลิก และไม่นับ excludeID)
func busyTables(ctx context.Context, q queryer, date string, startMin, excludeID int) (map[int]bool, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT table_id, booking_time FROM bookings WHERE booking_date = ? AND status <> 'cancelled' AND table_id IS NOT NULL AND id <> ?",
		date, excludeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	busy := map[int]bool{}
	for rows.Next() {
		var tid int
		var t string
		if err := rows.Scan(&tid, &t); err != nil {
			return nil, err
		}
		m, ok := parseClock(shortTime(t))
		if ok && timesOverlap(startMin, m) {
			busy[tid] = true
		}
	}
	return busy, rows.Err()
}

type tableCand struct{ ID, No int }

// chooseTable: เลือกจากรายการที่เรียงมาแล้ว (ที่นั่งน้อยสุดก่อน) ตัวแรกที่ไม่ว่าง
// ถ้ามี prefer และโต๊ะนั้นว่าง จะเลือกตัวนั้นก่อน
func chooseTable(cands []tableCand, busy map[int]bool, prefer int) (tableCand, bool) {
	var first *tableCand
	for i := range cands {
		c := cands[i]
		if busy[c.ID] {
			continue
		}
		if prefer > 0 && c.ID == prefer {
			return c, true
		}
		if first == nil {
			first = &cands[i]
		}
	}
	if first != nil {
		return *first, true
	}
	return tableCand{}, false
}

// pickTable: หาโต๊ะว่างที่เล็กที่สุดที่ใส่ size คนได้ในช่วงเวลานั้น คืน (table_id, table_no)
func pickTable(ctx context.Context, q queryer, date string, startMin, size, excludeID, prefer int) (int, int, error) {
	busy, err := busyTables(ctx, q, date, startMin, excludeID)
	if err != nil {
		return 0, 0, err
	}
	rows, err := q.QueryContext(ctx,
		"SELECT id, table_no FROM dining_tables WHERE is_active = 1 AND seats >= ? ORDER BY seats, table_no", size)
	if err != nil {
		return 0, 0, err
	}
	var cands []tableCand
	for rows.Next() {
		var c tableCand
		if err := rows.Scan(&c.ID, &c.No); err != nil {
			rows.Close()
			return 0, 0, err
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, err
	}
	rows.Close()

	if c, ok := chooseTable(cands, busy, prefer); ok {
		return c.ID, c.No, nil
	}

	var maxSeats int
	if err := q.QueryRowContext(ctx, "SELECT COALESCE(MAX(seats), 0) FROM dining_tables WHERE is_active = 1").Scan(&maxSeats); err != nil {
		return 0, 0, err
	}
	return 0, 0, &noTableError{TooBig: size > maxSeats, MaxSeats: maxSeats}
}

// createBooking: จัดโต๊ะให้อัตโนมัติแล้วบันทึกการจอง คืนหมายเลขโต๊ะที่ได้
func createBooking(ctx context.Context, userID int, date, tm string, startMin, size int, name, phone, request string) (int, error) {
	var tableNo int
	err := withDateLock(ctx, date, func(conn *sql.Conn) error {
		return withInventoryLock(ctx, func(inventoryConn *sql.Conn) error {
			tid, no, err := pickTable(ctx, inventoryConn, date, startMin, size, 0, 0)
			if err != nil {
				return err
			}
			_, err = inventoryConn.ExecContext(ctx,
				"INSERT INTO bookings(user_id, booking_date, booking_time, party_size, table_id, contact_name, contact_phone, special_request) VALUES(?, ?, ?, ?, ?, ?, ?, ?)",
				userID, date, tm, size, tid, name, phone, request)
			if err == nil {
				tableNo = no
			}
			return err
		})
	})
	return tableNo, err
}

// adminSetStatus: เปลี่ยนสถานะการจอง
// ถ้าเปิดรายการที่ยกเลิกไปแล้วกลับมา ต้องเช็คโต๊ะว่างอีกครั้ง (ใช้โต๊ะเดิมถ้ายังว่าง ไม่งั้นหาโต๊ะอื่นให้)
func adminSetStatus(ctx context.Context, id int, status string) error {
	var date, tm, old string
	var size int
	var tableID sql.NullInt64
	err := db.QueryRowContext(ctx,
		"SELECT booking_date, booking_time, party_size, table_id, status FROM bookings WHERE id = ?", id).
		Scan(&date, &tm, &size, &tableID, &old)
	if errors.Is(err, sql.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}

	if old != "cancelled" || status == "cancelled" {
		_, err = db.ExecContext(ctx, "UPDATE bookings SET status = ? WHERE id = ?", status, id)
		return err
	}

	date = shortDate(date)
	startMin, ok := parseClock(shortTime(tm))
	if !ok {
		return fmt.Errorf("เวลาของการจอง %d ไม่ถูกต้อง: %q", id, tm)
	}
	return withDateLock(ctx, date, func(conn *sql.Conn) error {
		return withInventoryLock(ctx, func(inventoryConn *sql.Conn) error {
			prefer := 0
			if tableID.Valid {
				prefer = int(tableID.Int64)
			}
			tid, _, err := pickTable(ctx, inventoryConn, date, startMin, size, id, prefer)
			if err != nil {
				return err
			}
			_, err = inventoryConn.ExecContext(ctx,
				"UPDATE bookings SET status = ?, table_id = ? WHERE id = ? AND status = 'cancelled'", status, tid, id)
			return err
		})
	})
}

// --------------------------------------------------------------------
// กระดานสถานะโต๊ะ
// --------------------------------------------------------------------

type TableStatus struct {
	ID        int
	No        int
	Seats     int
	Active    bool
	Occupied  bool
	BookingID int    // เฉพาะ admin
	Guest     string // เฉพาะ admin
	Phone     string // เฉพาะ admin
	Size      int    // เฉพาะ admin
	From      string // เวลาเริ่มของการจองที่ใช้โต๊ะนี้
	Until     string // โต๊ะจะว่างอีกครั้งเมื่อ
}

// boardParams: อ่านวันที่/เวลาที่ต้องการดูจาก ?date=&time= (ไม่ระบุ = ตอนนี้)
func boardParams(r *http.Request) (date, tm string, atMin int) {
	now := time.Now().In(thaiTZ)
	today := now.Format("2006-01-02")
	date = today
	if d := r.URL.Query().Get("date"); d != "" {
		if _, err := time.Parse("2006-01-02", d); err == nil {
			date = d
		}
	}
	if t := r.URL.Query().Get("time"); t != "" {
		if m, ok := parseClock(t); ok {
			return date, fmtClock(m), m
		}
	}
	if date == today {
		m := now.Hour()*60 + now.Minute()
		return date, fmtClock(m), m
	}
	return date, fmtClock(openMin), openMin
}

// tableBoard: สถานะโต๊ะทั้งหมด ณ วันที่/เวลาที่กำหนด
// admin=true จะเห็นโต๊ะที่ปิดใช้งานและรายละเอียดผู้จอง ลูกค้าเห็นแค่ว่าว่างหรือไม่
func tableBoard(ctx context.Context, date string, atMin int, admin bool) (tables []TableStatus, free, total int, err error) {
	rows, err := db.QueryContext(ctx, "SELECT id, table_no, seats, is_active FROM dining_tables ORDER BY table_no")
	if err != nil {
		return nil, 0, 0, err
	}
	idx := map[int]int{}
	for rows.Next() {
		var t TableStatus
		var active int
		if err := rows.Scan(&t.ID, &t.No, &t.Seats, &active); err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		t.Active = active == 1
		if !admin && !t.Active {
			continue
		}
		idx[t.ID] = len(tables)
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, 0, err
	}
	rows.Close()

	bRows, err := db.QueryContext(ctx,
		"SELECT id, table_id, booking_time, party_size, contact_name, contact_phone FROM bookings WHERE booking_date = ? AND status <> 'cancelled' AND table_id IS NOT NULL ORDER BY booking_time",
		date)
	if err != nil {
		return nil, 0, 0, err
	}
	defer bRows.Close()
	for bRows.Next() {
		var id, tid, size int
		var tm, name, phone string
		if err := bRows.Scan(&id, &tid, &tm, &size, &name, &phone); err != nil {
			return nil, 0, 0, err
		}
		m, ok := parseClock(shortTime(tm))
		if !ok || !(m <= atMin && atMin < m+diningMin) {
			continue
		}
		i, ok := idx[tid]
		if !ok {
			continue
		}
		t := &tables[i]
		t.Occupied = true
		t.From = fmtClock(m)
		t.Until = fmtClock(m + diningMin)
		if admin {
			t.BookingID, t.Guest, t.Phone, t.Size = id, name, phone, size
		}
	}
	if err := bRows.Err(); err != nil {
		return nil, 0, 0, err
	}

	for _, t := range tables {
		if t.Active {
			total++
			if !t.Occupied {
				free++
			}
		}
	}
	return tables, free, total, nil
}

// --------------------------------------------------------------------
// Admin: จัดการโต๊ะ
// --------------------------------------------------------------------

// futureBookingsOnTable: จำนวนการจองในอนาคต (ที่ยังไม่ยกเลิก) ของโต๊ะ ที่มีจำนวนคนมากกว่า minParty
func futureBookingsOnTable(ctx context.Context, q queryer, where string, arg interface{}, minParty int) (int, error) {
	nowStr := time.Now().In(thaiTZ).Format("2006-01-02 15:04:05")
	var n int
	err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM bookings b JOIN dining_tables t ON t.id = b.table_id WHERE "+where+
			" AND b.status <> 'cancelled' AND TIMESTAMP(b.booking_date, b.booking_time) > ? AND b.party_size > ?",
		arg, nowStr, minParty).Scan(&n)
	return n, err
}

// เพิ่มโต๊ะใหม่ หรือแก้จำนวนที่นั่งของโต๊ะเดิม (ถ้าหมายเลขโต๊ะซ้ำ)
func handleAdminTableSave(w http.ResponseWriter, r *http.Request) {
	if !postOnly(w, r) {
		return
	}
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	no, err := strconv.Atoi(r.FormValue("table_no"))
	if err != nil || no < 1 || no > 999 {
		http.Error(w, "หมายเลขโต๊ะต้องอยู่ระหว่าง 1-999", http.StatusBadRequest)
		return
	}
	seats, err := strconv.Atoi(r.FormValue("seats"))
	if err != nil || seats < 1 || seats > 50 {
		http.Error(w, "จำนวนที่นั่งต้องอยู่ระหว่าง 1-50", http.StatusBadRequest)
		return
	}

	var conflictCount int
	err = withInventoryLock(r.Context(), func(conn *sql.Conn) error {
		var err error
		conflictCount, err = futureBookingsOnTable(r.Context(), conn, "t.table_no = ?", no, seats)
		if err != nil || conflictCount > 0 {
			return err
		}
		_, err = conn.ExecContext(r.Context(),
			"INSERT INTO dining_tables(table_no, seats) VALUES(?, ?) ON DUPLICATE KEY UPDATE seats = VALUES(seats)", no, seats)
		return err
	})
	if err != nil {
		serverError(w, "table save", err)
		return
	}
	if conflictCount > 0 {
		http.Error(w, fmt.Sprintf("โต๊ะที่ %d มีการจองในอนาคตที่ใช้มากกว่า %d ที่นั่ง (%d รายการ) กรุณาย้ายหรือยกเลิกก่อนลดจำนวนที่นั่ง", no, seats, conflictCount), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/admin#tables", http.StatusSeeOther)
}

// เปิด/ปิดใช้งานโต๊ะ (ปิดไม่ได้ถ้ายังมีการจองในอนาคตค้างอยู่)
func handleAdminTableToggle(w http.ResponseWriter, r *http.Request) {
	if !postOnly(w, r) {
		return
	}
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	id, err := strconv.Atoi(r.FormValue("table_id"))
	if err != nil || id < 1 {
		http.Error(w, "รหัสโต๊ะไม่ถูกต้อง", http.StatusBadRequest)
		return
	}
	active := r.FormValue("active") == "1"

	v := 0
	if active {
		v = 1
	}
	var conflictCount int
	err = withInventoryLock(r.Context(), func(conn *sql.Conn) error {
		var err error
		if !active {
			conflictCount, err = futureBookingsOnTable(r.Context(), conn, "t.id = ?", id, 0)
			if err != nil || conflictCount > 0 {
				return err
			}
		}
		_, err = conn.ExecContext(r.Context(), "UPDATE dining_tables SET is_active = ? WHERE id = ?", v, id)
		return err
	})
	if err != nil {
		serverError(w, "table toggle", err)
		return
	}
	if conflictCount > 0 {
		http.Error(w, fmt.Sprintf("โต๊ะนี้ยังมีการจองในอนาคต %d รายการ กรุณาย้ายหรือยกเลิกก่อนปิดใช้งาน", conflictCount), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/admin#tables", http.StatusSeeOther)
}
