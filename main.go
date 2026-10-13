package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/mail"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	mysqldriver "github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
)

var (
	db        *sql.DB
	templates *template.Template
	// dummyHash ใช้เทียบรหัสผ่านเมื่อไม่พบอีเมล เพื่อให้เวลาตอบสนองใกล้เคียงกัน
	dummyHash []byte
	// เวลาประเทศไทย ใช้ตรวจว่าวันที่จองไม่ใช่อดีต
	thaiTZ = time.FixedZone("ICT", 7*60*60)

	// ตั้งค่าร้าน (อ่านจาก environment variable ใน loadConfig)
	diningMin  = 120     // ระยะเวลานั่งต่อการจอง 1 รายการ (นาที)
	openMin    = 10 * 60 // เวลาเปิดร้าน (นาทีนับจากเที่ยงคืน)
	lastMin    = 21 * 60 // เวลาจองได้ช้าสุด
	trustProxy = false   // เชื่อ X-Forwarded-* หรือไม่ (เปิดเมื่ออยู่หลัง reverse proxy ที่เราควบคุมเท่านั้น)
)

type Booking struct {
	ID             int
	UserID         int
	BookingDate    string
	BookingTime    string
	PartySize      int
	ContactName    string
	ContactPhone   string
	SpecialRequest string
	Status         string
	CanCancel      bool // ยังไม่ยกเลิก และยังไม่ถึงเวลาจอง
	TableNo        int  // หมายเลขโต๊ะ (0 = ยังไม่ได้จัดโต๊ะ เช่นรายการเก่า)
}

type Review struct {
	ID        int
	Rating    int
	Comment   string
	CreatedAt string
}

// --------------------------------------------------------------------
// Config
// --------------------------------------------------------------------

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		log.Fatalf("ค่า %s ไม่ถูกต้อง: %q", name, v)
	}
	return n
}

func parseClock(s string) (int, bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

func envClock(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	m, ok := parseClock(v)
	if !ok {
		log.Fatalf("ค่า %s ต้องอยู่ในรูปแบบ HH:MM แต่ได้ %q", name, v)
	}
	return m
}

func fmtClock(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

func loadConfig() {
	diningMin = envInt("DINING_MINUTES", diningMin)
	openMin = envClock("OPEN_TIME", openMin)
	lastMin = envClock("LAST_BOOKING_TIME", lastMin)
	trustProxy = os.Getenv("TRUST_PROXY") == "true"

	if openMin > lastMin {
		log.Fatal("OPEN_TIME ต้องไม่เกิน LAST_BOOKING_TIME")
	}
	// การเช็คความจุทำทีละวัน จึงห้ามให้ช่วงนั่งข้ามเที่ยงคืน
	if lastMin+diningMin > 24*60 {
		log.Fatal("LAST_BOOKING_TIME + DINING_MINUTES ต้องไม่เกินเที่ยงคืน")
	}
}

// --------------------------------------------------------------------
// Rate limiter (in-memory, fixed window)
// --------------------------------------------------------------------

type attempt struct {
	count int
	reset time.Time
}

type limiter struct {
	mu     sync.Mutex
	m      map[string]*attempt
	max    int
	window time.Duration
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{m: map[string]*attempt{}, max: max, window: window}
}

// blocked: ครบโควตาในช่วงเวลานี้แล้วหรือยัง (และต้องรออีกนานเท่าไร)
func (l *limiter) blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.m[key]
	if !ok || time.Now().After(a.reset) {
		return false, 0
	}
	if a.count >= l.max {
		return true, time.Until(a.reset)
	}
	return false, 0
}

// fail: นับเพิ่ม 1 ครั้ง
func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	a, ok := l.m[key]
	if !ok || now.After(a.reset) {
		l.m[key] = &attempt{count: 1, reset: now.Add(l.window)}
		return
	}
	a.count++
}

func (l *limiter) clear(key string) {
	l.mu.Lock()
	delete(l.m, key)
	l.mu.Unlock()
}

func (l *limiter) gc() {
	now := time.Now()
	l.mu.Lock()
	for k, a := range l.m {
		if now.After(a.reset) {
			delete(l.m, k)
		}
	}
	l.mu.Unlock()
}

var (
	loginIPLimiter   = newLimiter(20, 15*time.Minute) // ล็อกอินพลาดต่อ IP
	loginAcctLimiter = newLimiter(5, 15*time.Minute)  // ล็อกอินพลาดต่อ (IP + อีเมล)
	registerLimiter  = newLimiter(10, time.Hour)      // สมัครสมาชิกต่อ IP
)

// clientIP: ใช้ RemoteAddr เป็นหลัก ถ้าอยู่หลัง proxy ที่เชื่อถือได้ (TRUST_PROXY=true)
// ใช้ค่าท้ายสุดของ X-Forwarded-For ซึ่งเป็นค่าที่ proxy ของเราเติมเอง (ค่าก่อนหน้านั้นลูกค้าปลอมได้)
func clientIP(r *http.Request) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func tooManyRequests(w http.ResponseWriter, wait time.Duration) {
	secs := int(wait.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	mins := (secs + 59) / 60
	http.Error(w, fmt.Sprintf("พยายามมากเกินไป กรุณาลองใหม่ในอีกประมาณ %d นาที", mins), http.StatusTooManyRequests)
}

// --------------------------------------------------------------------
// Session (เก็บฝั่งเซิร์ฟเวอร์ ผู้ใช้แก้ user_id / role เองไม่ได้)
// หมายเหตุ: เก็บใน memory ถ้ารีสตาร์ทโปรแกรม ทุกคนจะต้องล็อกอินใหม่
// --------------------------------------------------------------------

const (
	sessionCookie = "session_id"
	csrfCookie    = "csrf_token"
	sessionTTL    = 12 * time.Hour
)

type session struct {
	UserID  int
	Role    string
	Expires time.Time
}

var (
	sessions   = map[string]session{}
	sessionsMu sync.RWMutex
)

func randomToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || (trustProxy && r.Header.Get("X-Forwarded-Proto") == "https")
}

func createSession(w http.ResponseWriter, r *http.Request, userID int, role string) error {
	token, err := randomToken(32)
	if err != nil {
		return err
	}
	sessionsMu.Lock()
	sessions[token] = session{UserID: userID, Role: role, Expires: time.Now().Add(sessionTTL)}
	sessionsMu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func getSession(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return session{}, false
	}
	sessionsMu.RLock()
	s, ok := sessions[c.Value]
	sessionsMu.RUnlock()
	if !ok || time.Now().After(s.Expires) {
		return session{}, false
	}
	return s, true
}

func destroySession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		sessionsMu.Lock()
		delete(sessions, c.Value)
		sessionsMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func cleanupSessions() {
	for range time.Tick(10 * time.Minute) {
		now := time.Now()
		sessionsMu.Lock()
		for k, s := range sessions {
			if now.After(s.Expires) {
				delete(sessions, k)
			}
		}
		sessionsMu.Unlock()

		loginIPLimiter.gc()
		loginAcctLimiter.gc()
		registerLimiter.gc()
	}
}

// --------------------------------------------------------------------
// CSRF (double-submit cookie: ค่าใน cookie ต้องตรงกับ hidden field ในฟอร์ม)
// --------------------------------------------------------------------

func ensureCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) == 64 {
		return c.Value
	}
	tok, err := randomToken(32)
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	return tok
}

func checkCSRF(r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		return false
	}
	form := r.FormValue("csrf_token")
	if form == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(form)) == 1
}

// postOnly: ยอมรับเฉพาะ POST ที่มี CSRF token ถูกต้อง
func postOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !checkCSRF(r) {
		http.Error(w, "คำขอไม่ถูกต้อง (CSRF) กรุณารีเฟรชหน้าแล้วลองใหม่", http.StatusForbidden)
		return false
	}
	return true
}

// --------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------

func requireUser(w http.ResponseWriter, r *http.Request) (session, bool) {
	s, ok := getSession(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return session{}, false
	}
	return s, true
}

func requireAdmin(w http.ResponseWriter, r *http.Request) (session, bool) {
	s, ok := requireUser(w, r)
	if !ok {
		return session{}, false
	}
	if s.Role != "admin" {
		http.Error(w, "Forbidden: สำหรับ Admin เท่านั้น", http.StatusForbidden)
		return session{}, false
	}
	return s, true
}

func serverError(w http.ResponseWriter, context string, err error) {
	log.Printf("ERROR %s: %v", context, err)
	http.Error(w, "เกิดข้อผิดพลาดภายในระบบ กรุณาลองใหม่อีกครั้ง", http.StatusInternalServerError)
}

func render(w http.ResponseWriter, r *http.Request, name string, data map[string]interface{}) {
	if data == nil {
		data = map[string]interface{}{}
	}
	data["CSRFToken"] = ensureCSRF(w, r)

	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name+".html", data); err != nil {
		serverError(w, "render "+name, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

func validEmail(email string) bool {
	if email == "" || len(email) > 254 {
		return false
	}
	addr, err := mail.ParseAddress(email)
	return err == nil && addr.Address == email
}

var phoneRe = regexp.MustCompile(`^0\d{8,9}$`)

// normalizePhone: รับเบอร์ไทย เช่น 081-234-5678, 0812345678, +66812345678
// คืนค่ารูปแบบ 0XXXXXXXXX (9-10 หลัก)
func normalizePhone(s string) (string, bool) {
	s = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(strings.TrimSpace(s))
	if strings.HasPrefix(s, "+66") {
		s = "0" + s[3:]
	}
	if !phoneRe.MatchString(s) {
		return "", false
	}
	return s, true
}

// ตัดรูปแบบวันที่/เวลาจาก DB ให้อ่านง่าย (เช่น 2026-10-10T00:00:00Z -> 2026-10-10)
func shortDate(s string) string {
	if len(s) >= 10 && s[4] == '-' {
		return s[:10]
	}
	return s
}

func shortTime(s string) string {
	if len(s) > 5 && s[2] == ':' {
		return s[:5]
	}
	return s
}

func shortDateTime(s string) string {
	if len(s) >= 19 && s[4] == '-' {
		return strings.Replace(s[:19], "T", " ", 1)
	}
	return s
}

const (
	bookingCols = "b.id, b.user_id, b.booking_date, b.booking_time, b.party_size, b.contact_name, b.contact_phone, b.special_request, b.status, COALESCE(t.table_no, 0)"
	bookingFrom = "FROM bookings b LEFT JOIN dining_tables t ON t.id = b.table_id"
)

func scanBookings(rows *sql.Rows) ([]Booking, error) {
	defer rows.Close()
	now := time.Now().In(thaiTZ)
	var out []Booking
	for rows.Next() {
		var b Booking
		var date, tm string
		var sr sql.NullString
		if err := rows.Scan(&b.ID, &b.UserID, &date, &tm, &b.PartySize, &b.ContactName, &b.ContactPhone, &sr, &b.Status, &b.TableNo); err != nil {
			return nil, err
		}
		b.BookingDate = shortDate(date)
		b.BookingTime = shortTime(tm)
		b.SpecialRequest = sr.String
		if bt, err := time.ParseInLocation("2006-01-02 15:04", b.BookingDate+" "+b.BookingTime, thaiTZ); err == nil {
			b.CanCancel = b.Status != "cancelled" && bt.After(now)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// --------------------------------------------------------------------
// Main
// --------------------------------------------------------------------

func main() {
	// ตั้งค่าได้ผ่าน environment variable เช่น
	//   DB_DSN="restaurant_app:รหัสผ่าน@tcp(127.0.0.1:3306)/restaurant_db"
	//   ADDR=":8080"
	//   DINING_MINUTES=120  OPEN_TIME=10:00  LAST_BOOKING_TIME=21:00
	//   TRUST_PROXY=true   (เฉพาะเมื่ออยู่หลัง reverse proxy ของเราเอง)
	loadConfig()

	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		dsn = "root:@tcp(127.0.0.1:3306)/restaurant_db"
		log.Println("คำเตือน: ใช้ DB user 'root' ไม่มีรหัสผ่าน (เหมาะกับเครื่องทดสอบเท่านั้น) ตั้งค่า DB_DSN เพื่อใช้ user เฉพาะแอป")
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// บังคับ parseTime + เวลาไทย ไม่ว่า DSN จะระบุมาอย่างไร
	// และตั้ง time_zone ของ session MySQL ให้เป็น +07:00 เพื่อให้ CURRENT_TIMESTAMP ตรงกัน
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		log.Fatal("DB_DSN ไม่ถูกต้อง:", err)
	}
	cfg.ParseTime = true
	cfg.Loc = thaiTZ
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["time_zone"] = "'+07:00'"

	connector, err := mysqldriver.NewConnector(cfg)
	if err != nil {
		log.Fatal("Connection error:", err)
	}
	db = sql.OpenDB(connector)
	defer db.Close()
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		log.Fatal("Database ping error:", err)
	}
	fmt.Println("Connected to restaurant_db successfully!")

	templates, err = template.New("").Funcs(template.FuncMap{
		"sub": func(a, b int) int { return a - b },
	}).ParseGlob("templates/*.html")
	if err != nil {
		log.Fatal("Template error:", err)
	}

	dummyHash, err = bcrypt.GenerateFromPassword([]byte("dummy-password"), 12)
	if err != nil {
		log.Fatal(err)
	}

	go cleanupSessions()

	mux := http.NewServeMux()

	// Auth
	mux.HandleFunc("/register", handleRegister)
	mux.HandleFunc("/login", handleLogin)
	mux.HandleFunc("/logout", handleLogout)

	// Customer
	mux.HandleFunc("/", handleHome)
	mux.HandleFunc("/booking/create", handleCreateBooking)
	mux.HandleFunc("/booking/cancel", handleCancelBooking)
	mux.HandleFunc("/review/create", handleCreateReview)

	// Admin
	mux.HandleFunc("/admin", handleAdminDashboard)
	mux.HandleFunc("/admin/booking/update", handleAdminUpdateBooking)
	mux.HandleFunc("/admin/table/save", handleAdminTableSave)
	mux.HandleFunc("/admin/table/toggle", handleAdminTableToggle)

	srv := &http.Server{
		Addr:              addr,
		Handler:           secure(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	fmt.Println("Server running on http://localhost" + addr)
	log.Fatal(srv.ListenAndServe())
}

// secure: ใส่ security header พื้นฐาน และจำกัดขนาด request body
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB
		next.ServeHTTP(w, r)
	})
}

// --------------------------------------------------------------------
// Auth handlers
// --------------------------------------------------------------------

func handleRegister(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		render(w, r, "register", nil)
	case http.MethodPost:
		if !postOnly(w, r) {
			return
		}
		// bcrypt cost 12 กิน CPU จึงจำกัดจำนวนครั้งต่อ IP
		ip := clientIP(r)
		if blocked, wait := registerLimiter.blocked(ip); blocked {
			tooManyRequests(w, wait)
			return
		}
		registerLimiter.fail(ip)

		email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
		password := r.FormValue("password")

		if !validEmail(email) {
			http.Error(w, "รูปแบบอีเมลไม่ถูกต้อง", http.StatusBadRequest)
			return
		}
		// bcrypt รองรับรหัสผ่านสูงสุด 72 ไบต์
		if len(password) < 8 || len(password) > 72 {
			http.Error(w, "รหัสผ่านต้องยาว 8-72 ตัวอักษร", http.StatusBadRequest)
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
		if err != nil {
			serverError(w, "bcrypt", err)
			return
		}

		_, err = db.Exec("INSERT INTO users(email, password_hash, role) VALUES(?, ?, 'customer')", email, string(hash))
		if err != nil {
			var me *mysqldriver.MySQLError
			if errors.As(err, &me) && me.Number == 1062 { // duplicate entry
				http.Error(w, "Email นี้ถูกใช้งานแล้ว", http.StatusBadRequest)
				return
			}
			serverError(w, "register insert", err)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		render(w, r, "login", nil)
	case http.MethodPost:
		if !postOnly(w, r) {
			return
		}
		email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
		password := r.FormValue("password")

		// จำกัดจำนวนครั้งที่ล็อกอินพลาด ก่อนจะทำ query/bcrypt
		// นับต่อ IP และต่อ (IP+อีเมล) เพื่อไม่ให้คนอื่นล็อกบัญชีเหยื่อจากเครื่องอื่นได้
		ip := clientIP(r)
		ipKey := ip
		acctKey := ip + "|" + email
		if blocked, wait := loginIPLimiter.blocked(ipKey); blocked {
			tooManyRequests(w, wait)
			return
		}
		if blocked, wait := loginAcctLimiter.blocked(acctKey); blocked {
			tooManyRequests(w, wait)
			return
		}
		fail := func() {
			loginIPLimiter.fail(ipKey)
			loginAcctLimiter.fail(acctKey)
		}

		var id int
		var storedHash, role string
		err := db.QueryRowContext(r.Context(), "SELECT id, password_hash, role FROM users WHERE email = ?", email).Scan(&id, &storedHash, &role)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				serverError(w, "login query", err)
				return
			}
			// ไม่พบผู้ใช้: เทียบกับ dummy hash เพื่อไม่ให้เวลาตอบต่างกันจนเดาอีเมลได้
			bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
			fail()
			http.Error(w, "อีเมลหรือรหัสผ่านไม่ถูกต้อง", http.StatusUnauthorized)
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(password)) != nil {
			fail()
			http.Error(w, "อีเมลหรือรหัสผ่านไม่ถูกต้อง", http.StatusUnauthorized)
			return
		}
		loginAcctLimiter.clear(acctKey)

		// ล้าง session เก่า (ถ้ามี) แล้วสร้างใหม่ทุกครั้งที่ล็อกอิน
		destroySession(w, r)
		if err := createSession(w, r, id, role); err != nil {
			serverError(w, "create session", err)
			return
		}

		if role == "admin" {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/", http.StatusSeeOther)
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// ออกจากระบบต้องเป็น POST + CSRF token กันเว็บอื่นแอบสั่ง logout ผ่าน <img src="/logout">
func handleLogout(w http.ResponseWriter, r *http.Request) {
	if !postOnly(w, r) {
		return
	}
	destroySession(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --------------------------------------------------------------------
// Customer handlers
// --------------------------------------------------------------------

func handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s, ok := requireUser(w, r)
	if !ok {
		return
	}

	// รายการจองของผู้ใช้คนนี้เท่านั้น (Ownership)
	rows, err := db.QueryContext(r.Context(), "SELECT "+bookingCols+" "+bookingFrom+" WHERE b.user_id = ? ORDER BY b.id DESC", s.UserID)
	if err != nil {
		serverError(w, "home bookings query", err)
		return
	}
	bookings, err := scanBookings(rows)
	if err != nil {
		serverError(w, "home bookings scan", err)
		return
	}

	rRows, err := db.QueryContext(r.Context(), "SELECT id, rating, comment, created_at FROM reviews ORDER BY id DESC")
	if err != nil {
		serverError(w, "home reviews query", err)
		return
	}
	defer rRows.Close()
	var reviews []Review
	for rRows.Next() {
		var rev Review
		var created string
		if err := rRows.Scan(&rev.ID, &rev.Rating, &rev.Comment, &created); err != nil {
			serverError(w, "home reviews scan", err)
			return
		}
		rev.CreatedAt = shortDateTime(created)
		reviews = append(reviews, rev)
	}
	if err := rRows.Err(); err != nil {
		serverError(w, "home reviews rows", err)
		return
	}

	// สถานะโต๊ะ ณ วันที่/เวลาที่เลือก (ค่าเริ่มต้น = ตอนนี้)
	checkDate, checkTime, atMin := boardParams(r)
	tables, free, total, err := tableBoard(r.Context(), checkDate, atMin, false)
	if err != nil {
		serverError(w, "home table board", err)
		return
	}

	render(w, r, "home", map[string]interface{}{
		"UserID":     s.UserID,
		"UserRole":   s.Role,
		"Bookings":   bookings,
		"Reviews":    reviews,
		"Today":      time.Now().In(thaiTZ).Format("2006-01-02"),
		"OpenTime":   fmtClock(openMin),
		"LastTime":   fmtClock(lastMin),
		"Tables":     tables,
		"FreeCount":  free,
		"TotalCount": total,
		"CheckDate":  checkDate,
		"CheckTime":  checkTime,
	})
}

// ฟอร์มที่ 1: สร้างรายการจอง
func handleCreateBooking(w http.ResponseWriter, r *http.Request) {
	if !postOnly(w, r) {
		return
	}
	s, ok := requireUser(w, r)
	if !ok {
		return
	}

	date := strings.TrimSpace(r.FormValue("booking_date"))
	tm := strings.TrimSpace(r.FormValue("booking_time"))
	sizeStr := strings.TrimSpace(r.FormValue("party_size"))
	name := strings.TrimSpace(r.FormValue("contact_name"))
	phoneRaw := r.FormValue("contact_phone")
	request := strings.TrimSpace(r.FormValue("special_request"))

	d, err := time.ParseInLocation("2006-01-02", date, thaiTZ)
	if err != nil {
		http.Error(w, "รูปแบบวันที่ไม่ถูกต้อง", http.StatusBadRequest)
		return
	}
	startMin, okTime := parseClock(tm)
	if !okTime {
		http.Error(w, "รูปแบบเวลาไม่ถูกต้อง", http.StatusBadRequest)
		return
	}

	// เช็ควันที่ "และเวลา" รวมกัน ไม่ให้จองเวลาที่ผ่านมาแล้วของวันนี้
	now := time.Now().In(thaiTZ)
	bookingAt := d.Add(time.Duration(startMin) * time.Minute)
	if bookingAt.Before(now) {
		http.Error(w, "ไม่สามารถจองวันและเวลาที่ผ่านมาแล้วได้", http.StatusBadRequest)
		return
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, thaiTZ)
	if d.After(today.AddDate(1, 0, 0)) {
		http.Error(w, "จองล่วงหน้าได้ไม่เกิน 1 ปี", http.StatusBadRequest)
		return
	}
	if startMin < openMin || startMin > lastMin {
		http.Error(w, fmt.Sprintf("รับจองเวลา %s-%s น. เท่านั้น", fmtClock(openMin), fmtClock(lastMin)), http.StatusBadRequest)
		return
	}

	size, err := strconv.Atoi(sizeStr)
	if err != nil || size < 1 || size > 50 {
		http.Error(w, "จำนวนคนต้องอยู่ระหว่าง 1-50", http.StatusBadRequest)
		return
	}
	if n := utf8.RuneCountInString(name); n < 1 || n > 100 {
		http.Error(w, "กรุณากรอกชื่อผู้จอง (ไม่เกิน 100 ตัวอักษร)", http.StatusBadRequest)
		return
	}
	phone, okPhone := normalizePhone(phoneRaw)
	if !okPhone {
		http.Error(w, "เบอร์โทรไม่ถูกต้อง (เช่น 0812345678)", http.StatusBadRequest)
		return
	}
	if utf8.RuneCountInString(request) > 500 {
		http.Error(w, "หมายเหตุยาวเกิน 500 ตัวอักษร", http.StatusBadRequest)
		return
	}

	_, err = createBooking(r.Context(), s.UserID, date, tm, startMin, size, name, phone, request)
	if err != nil {
		var nt *noTableError
		switch {
		case errors.As(err, &nt):
			http.Error(w, noTableMessage(nt, size), http.StatusConflict)
		case errors.Is(err, errBookingBusy):
			http.Error(w, "ระบบกำลังประมวลผลการจองอื่นอยู่ กรุณาลองใหม่อีกครั้ง", http.StatusServiceUnavailable)
		default:
			serverError(w, "create booking", err)
		}
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ยกเลิกการจอง (เช็คเจ้าของด้วย user_id จาก session)
// ยกเลิกได้เฉพาะรายการที่ยังไม่ยกเลิก และยังไม่ถึงเวลาจอง
func handleCancelBooking(w http.ResponseWriter, r *http.Request) {
	if !postOnly(w, r) {
		return
	}
	s, ok := requireUser(w, r)
	if !ok {
		return
	}

	bookingID, err := strconv.Atoi(r.FormValue("booking_id"))
	if err != nil || bookingID < 1 {
		http.Error(w, "รหัสการจองไม่ถูกต้อง", http.StatusBadRequest)
		return
	}

	nowStr := time.Now().In(thaiTZ).Format("2006-01-02 15:04:05")
	res, err := db.ExecContext(r.Context(),
		"UPDATE bookings SET status = 'cancelled' WHERE id = ? AND user_id = ? AND status <> 'cancelled' AND TIMESTAMP(booking_date, booking_time) > ?",
		bookingID, s.UserID, nowStr)
	if err != nil {
		serverError(w, "cancel booking", err)
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		serverError(w, "cancel booking rows", err)
		return
	}
	if n == 0 {
		http.Error(w, "ไม่พบรายการจองนี้ หรือถูกยกเลิกไปแล้ว หรือเลยเวลาจองแล้ว", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ฟอร์มที่ 2: ส่งรีวิว
func handleCreateReview(w http.ResponseWriter, r *http.Request) {
	if !postOnly(w, r) {
		return
	}
	s, ok := requireUser(w, r)
	if !ok {
		return
	}

	rating, err := strconv.Atoi(r.FormValue("rating"))
	if err != nil || rating < 1 || rating > 5 {
		http.Error(w, "คะแนนต้องอยู่ระหว่าง 1-5", http.StatusBadRequest)
		return
	}
	comment := strings.TrimSpace(r.FormValue("comment"))
	if comment == "" || utf8.RuneCountInString(comment) > 1000 {
		http.Error(w, "ความคิดเห็นต้องมี 1-1000 ตัวอักษร", http.StatusBadRequest)
		return
	}

	_, err = db.ExecContext(r.Context(), "INSERT INTO reviews(user_id, rating, comment) VALUES(?, ?, ?)", s.UserID, rating, comment)
	if err != nil {
		serverError(w, "create review", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --------------------------------------------------------------------
// Admin handlers
// --------------------------------------------------------------------

func handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}

	rows, err := db.QueryContext(r.Context(), "SELECT "+bookingCols+" "+bookingFrom+" ORDER BY b.id DESC")
	if err != nil {
		serverError(w, "admin bookings query", err)
		return
	}
	bookings, err := scanBookings(rows)
	if err != nil {
		serverError(w, "admin bookings scan", err)
		return
	}

	checkDate, checkTime, atMin := boardParams(r)
	tables, free, total, err := tableBoard(r.Context(), checkDate, atMin, true)
	if err != nil {
		serverError(w, "admin table board", err)
		return
	}

	render(w, r, "admin", map[string]interface{}{
		"Bookings":   bookings,
		"Tables":     tables,
		"FreeCount":  free,
		"TotalCount": total,
		"CheckDate":  checkDate,
		"CheckTime":  checkTime,
	})
}

func handleAdminUpdateBooking(w http.ResponseWriter, r *http.Request) {
	if !postOnly(w, r) {
		return
	}
	if _, ok := requireAdmin(w, r); !ok {
		return
	}

	bookingID, err := strconv.Atoi(r.FormValue("booking_id"))
	if err != nil || bookingID < 1 {
		http.Error(w, "รหัสการจองไม่ถูกต้อง", http.StatusBadRequest)
		return
	}
	status := r.FormValue("status")
	switch status {
	case "confirmed", "pending", "cancelled":
	default:
		http.Error(w, "สถานะไม่ถูกต้อง", http.StatusBadRequest)
		return
	}

	if err := adminSetStatus(r.Context(), bookingID, status); err != nil {
		var nt *noTableError
		switch {
		case errors.Is(err, errNotFound):
			http.Error(w, "ไม่พบรายการจองนี้", http.StatusNotFound)
		case errors.As(err, &nt):
			http.Error(w, "เปิดการจองนี้กลับมาไม่ได้ เพราะช่วงเวลานั้นไม่มีโต๊ะว่างที่รองรับจำนวนคน", http.StatusConflict)
		case errors.Is(err, errBookingBusy):
			http.Error(w, "ระบบกำลังประมวลผลการจองอื่นอยู่ กรุณาลองใหม่อีกครั้ง", http.StatusServiceUnavailable)
		default:
			serverError(w, "admin update booking", err)
		}
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}
