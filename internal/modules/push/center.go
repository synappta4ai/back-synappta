package push

import (
	"database/sql"
	"time"
)

// CenterNotification is a persistent per-user alert (video ready/failed…).
// It feeds the app's native bell and honors the same opt-in as push.
type CenterNotification struct {
	ID        string     `json:"id"`
	Type      string     `json:"type"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	TaskID    string     `json:"task_id"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// CenterStore persists alerts in the tenant schema.
type CenterStore struct {
	db *sql.DB
}

func NewCenterStore(db *sql.DB) *CenterStore { return &CenterStore{db: db} }

// Record appends an alert for a user.
func (s *CenterStore) Record(userID int64, ntype, title, body, taskID string) error {
	_, err := s.db.Exec(`INSERT INTO user_notifications (user_id, type, title, body, task_id)
		VALUES ($1, $2, $3, $4, $5)`, userID, ntype, title, body, taskID)
	return err
}

// List returns the newest alerts for a user.
func (s *CenterStore) List(userID int64, limit int) ([]CenterNotification, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := s.db.Query(`SELECT id, type, title, body, task_id, read_at, created_at
		FROM user_notifications WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CenterNotification
	for rows.Next() {
		var n CenterNotification
		var readAt sql.NullTime
		if err := rows.Scan(&n.ID, &n.Type, &n.Title, &n.Body, &n.TaskID, &readAt, &n.CreatedAt); err != nil {
			return nil, err
		}
		if readAt.Valid {
			t := readAt.Time
			n.ReadAt = &t
		}
		out = append(out, n)
	}
	if out == nil {
		out = []CenterNotification{}
	}
	return out, rows.Err()
}

// UnreadCount counts alerts the user hasn't opened yet.
func (s *CenterStore) UnreadCount(userID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM user_notifications
		WHERE user_id = $1 AND read_at IS NULL`, userID).Scan(&n)
	return n, err
}

// MarkAllRead marks every alert as read, returning the count.
func (s *CenterStore) MarkAllRead(userID int64) (int64, error) {
	res, err := s.db.Exec(`UPDATE user_notifications SET read_at = NOW()
		WHERE user_id = $1 AND read_at IS NULL`, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ─── Service ────────────────────────────────────────────────────

// CenterService records completion alerts and serves the native bell.
type CenterService struct {
	store *CenterStore
}

func NewCenterService(db *sql.DB) *CenterService {
	return &CenterService{store: NewCenterStore(db)}
}

// Record stores one alert; failures are reported so the core can log them.
func (s *CenterService) Record(userID int64, ntype, title, body, taskID string) error {
	return s.store.Record(userID, ntype, title, body, taskID)
}

func (s *CenterService) List(userID int64, limit int) ([]CenterNotification, error) {
	return s.store.List(userID, limit)
}

func (s *CenterService) UnreadCount(userID int64) (int, error) {
	return s.store.UnreadCount(userID)
}

func (s *CenterService) MarkAllRead(userID int64) (int64, error) {
	return s.store.MarkAllRead(userID)
}
