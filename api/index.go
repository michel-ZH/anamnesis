// api/index.go
package handler

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-spaced-repetition/go-fsrs/v3"
)

type application struct {
	db   *pgxpool.Pool
	tmpl *template.Template
}

type dbConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	KairosDB string
	SSLMode  string
}

type Card struct {
	Headword   string    `db:"headword"`
	Pinyin     string    `db:"pinyin"`
	EnDef      string    `db:"en_def"`
	ZhDef      string    `db:"zh_def"`
	Freq       int       `db:"freq"`
	Stability  float64   `db:"stability"`
	Difficulty float64   `db:"difficulty"`
	Lapses     int       `db:"lapses"`
	State      int       `db:"state"`
	LastReview time.Time `db:"last_review"`
	Due        time.Time `db:"due_at"`
	Reps       int       `db:"reps_ct"`
}

const cardQuery = `
select
headword, pinyin,
english_definition as en_def,
chinese_definition as zh_def,
coalesce(freq, 0) as freq,
stability, difficulty, lapses, state,
last_review,
due_at,
reps_ct
from entries
`

const (
	nextDueQuery    = cardQuery + ` where now() >= due_at order by due_at asc limit 1`
	byHeadwordQuery = cardQuery + ` where headword = $1`
)

func (c Card) mapToFSRS() fsrs.Card {
	return fsrs.Card{
		Stability:     c.Stability,
		Difficulty:    c.Difficulty,
		ElapsedDays:   uint64(time.Since(c.LastReview).Hours() / 24),
		ScheduledDays: uint64(math.Round(c.Due.Sub(c.LastReview).Hours() / 24)),
		Reps:          uint64(c.Reps),
		Lapses:        uint64(c.Lapses),
		State:         fsrs.State(c.State),
		LastReview:    c.LastReview,
	}
}

func getenvRequired(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("missing required environment variable %s", key)
	}
	return v, nil
}

func getenvDefault(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

func loadDBConfigFromEnv() (dbConfig, error) {
	host, err := getenvRequired("PGHOST")
	if err != nil {
		return dbConfig{}, err
	}
	port := getenvDefault("PGPORT", "5432")
	user, err := getenvRequired("PGUSER")
	if err != nil {
		return dbConfig{}, err
	}
	pass, err := getenvRequired("PGPASSWORD")
	if err != nil {
		return dbConfig{}, err
	}
	kairosDB, err := getenvRequired("KAIROS_DB")
	if err != nil {
		return dbConfig{}, err
	}
	sslmode := getenvDefault("PGSSLMODE", "require")
	return dbConfig{
		Host:     host,
		Port:     port,
		User:     user,
		Password: pass,
		KairosDB: kairosDB,
		SSLMode:  sslmode,
	}, nil
}

func buildPostgresURL(cfg dbConfig, dbName string) (string, error) {
	if dbName == "" {
		return "", errors.New("dbName is empty")
	}
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.User, cfg.Password),
		Host:   net.JoinHostPort(cfg.Host, cfg.Port),
		Path:   "/" + dbName,
	}
	q := u.Query()
	q.Set("sslmode", cfg.SSLMode)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func getNextDueCard(pool *pgxpool.Pool) (*Card, error) {
	ctx := context.Background()
	row := pool.QueryRow(ctx, nextDueQuery)
	var c Card
	err := row.Scan(&c.Headword, &c.Pinyin, &c.EnDef, &c.ZhDef, &c.Freq, &c.Stability, &c.Difficulty, &c.Lapses, &c.State, &c.LastReview, &c.Due, &c.Reps)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

func getCardByHeadword(pool *pgxpool.Pool, headword string) (*Card, error) {
	ctx := context.Background()
	row := pool.QueryRow(ctx, byHeadwordQuery, headword)
	var c Card
	err := row.Scan(&c.Headword, &c.Pinyin, &c.EnDef, &c.ZhDef, &c.Freq, &c.Stability, &c.Difficulty, &c.Lapses, &c.State, &c.LastReview, &c.Due, &c.Reps)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

func updateCardInDB(pool *pgxpool.Pool, c Card) error {
	const updateSQL = `
update entries set
stability = $1,
difficulty = $2,
lapses = $3,
state = $4,
last_review = $5,
due_at = $6,
reps_ct = $7
where headword = $8
`
	_, err := pool.Exec(context.Background(), updateSQL,
		c.Stability,
		c.Difficulty,
		c.Lapses,
		c.State,
		c.LastReview,
		c.Due,
		c.Reps,
		c.Headword,
	)
	return err
}

func (app *application) handleReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	card, err := getNextDueCard(app.db)
	if err != nil {
		http.Error(w, "DB error", http.StatusInternalServerError)
		return
	}
	if card == nil {
		w.Write([]byte("<h1>All cards reviewed!</h1>"))
		return
	}
	if err := app.tmpl.ExecuteTemplate(w, "front.html", card); err != nil {
		http.Error(w, "Template error", http.StatusInternalServerError)
	}
}

func (app *application) handleReveal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Form parse error", http.StatusBadRequest)
		return
	}
	headword := r.FormValue("front")
	if headword == "" {
		http.Redirect(w, r, "/review", http.StatusSeeOther)
		return
	}
	card, err := getCardByHeadword(app.db, headword)
	if err != nil {
		http.Error(w, "DB error", http.StatusInternalServerError)
		return
	}
	if card == nil {
		http.Redirect(w, r, "/review", http.StatusSeeOther)
		return
	}
	if err := app.tmpl.ExecuteTemplate(w, "back.html", card); err != nil {
		http.Error(w, "Template error", http.StatusInternalServerError)
	}
}

func (app *application) handleGrade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Form parse error", http.StatusBadRequest)
		return
	}
	headword := r.FormValue("front")
	if headword == "" {
		http.Error(w, "Sync error: refresh page", http.StatusBadRequest)
		return
	}
	currentCard, err := getCardByHeadword(app.db, headword)
	if err != nil {
		http.Error(w, "DB error", http.StatusInternalServerError)
		return
	}
	if currentCard == nil {
		http.Error(w, "Card not found", http.StatusNotFound)
		return
	}
	if r.FormValue("front") != currentCard.Headword {
		http.Error(w, "Sync error: refresh page", http.StatusBadRequest)
		return
	}
	ratingInt, err := strconv.Atoi(r.FormValue("rating"))
	if err != nil {
		http.Error(w, "Invalid rating", http.StatusBadRequest)
		return
	}
	grade := fsrs.Rating(ratingInt)
	p := fsrs.DefaultParam()
	f := fsrs.NewFSRS(p)
	now := time.Now()
	scheduledCards := f.Repeat(currentCard.mapToFSRS(), now)
	result := scheduledCards[grade].Card
	currentCard.Stability = result.Stability
	currentCard.Difficulty = result.Difficulty
	currentCard.State = int(result.State)
	currentCard.Lapses = int(result.Lapses)
	currentCard.LastReview = result.LastReview
	currentCard.Due = result.Due
	currentCard.Reps = int(result.Reps)
	if err := updateCardInDB(app.db, *currentCard); err != nil {
		http.Error(w, "Save failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/review", http.StatusSeeOther)
}

var (
	app  *application
	once sync.Once
)

func initApp() {
	cfg, err := loadDBConfigFromEnv()
	if err != nil {
		panic(fmt.Sprintf("Config error: %v", err))
	}
	kairosURL, err := buildPostgresURL(cfg, cfg.KairosDB)
	if err != nil {
		panic(fmt.Sprintf("DB URL error: %v", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dbPool, err := pgxpool.New(ctx, kairosURL)
	if err != nil {
		panic(fmt.Sprintf("DB connect error: %v", err))
	}
	tmpl := template.Must(template.ParseGlob("../templates/*.html")) // Adjust path if needed

	app = &application{
		db:   dbPool,
		tmpl: tmpl,
	}
}

func Handler(w http.ResponseWriter, r *http.Request) {
	once.Do(initApp)
	mux := http.NewServeMux()
	mux.HandleFunc("/review", app.handleReview)
	mux.HandleFunc("/reveal", app.handleReveal)
	mux.HandleFunc("/grade", app.handleGrade)
	mux.ServeHTTP(w, r)
}

func main() {}
