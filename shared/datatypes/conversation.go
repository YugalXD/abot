package dt

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"time"

	log "github.com/avabot/ava/Godeps/_workspace/src/github.com/Sirupsen/logrus"
	"github.com/avabot/ava/Godeps/_workspace/src/github.com/dchest/stemmer/porter2"
	"github.com/avabot/ava/Godeps/_workspace/src/github.com/jbrukh/bayesian"
	"github.com/avabot/ava/Godeps/_workspace/src/github.com/jmoiron/sqlx"
	"github.com/avabot/ava/shared/nlp"
)

var (
	ErrMissingUser = errors.New("missing user")
)

type jsonState json.RawMessage

type Msg struct {
	ID                uint64
	FlexID            string
	FlexIDType        int
	Sentence          string
	SentenceAnnotated string
	User              *User
	StructuredInput   *nlp.StructuredInput
	Stems             []string
	Route             string
	Package           string
	State             map[string]interface{}
	// AvaSent determines if msg is from the user or Ava
	AvaSent bool
	// SentenceFields breaks the sentence into words. Tokens like ,.' are
	// treated as individual words.
	SentenceFields []string
}

// RespMsg is used to pass results from packages to Ava
type RespMsg struct {
	MsgID    uint64
	Sentence string
}

type Feedback struct {
	Id        uint64
	Sentence  string
	Sentiment int
	CreatedAt time.Time
}

const (
	SentimentNegative = -1
	SentimentNeutral  = 0
	SentimentPositive = 1
)

func (j *jsonState) Scan(value interface{}) error {
	if err := json.Unmarshal(value.([]byte), *j); err != nil {
		log.Println("unmarshal jsonState: ", err)
		return err
	}
	return nil
}

func (j *jsonState) Value() (driver.Value, error) {
	return j, nil
}

func NewMsg(db *sqlx.DB, bayes *bayesian.Classifier, u *User, cmd string) *Msg {
	words := strings.Fields(cmd)
	eng := porter2.Stemmer
	stems := []string{}
	for _, w := range words {
		w = strings.TrimRight(w, ",.?;:!-/")
		stems = append(stems, eng.Stem(w))
	}
	// TODO handle training here with the _ var
	var si *nlp.StructuredInput
	var annotated string
	var err error
	if bayes != nil {
		si, annotated, _, err = nlp.Classify(bayes, cmd)
		if err != nil {
			log.Errorln("classifying sentence", err)
		}
	}
	m := &Msg{
		User:              u,
		Sentence:          cmd,
		SentenceFields:    SentenceFields(cmd),
		Stems:             stems,
		StructuredInput:   si,
		SentenceAnnotated: annotated,
	}
	m, err = addContext(db, m)
	if err != nil {
		log.WithField("fn", "addContext").Errorln(err)
	}
	return m
}

func GetMsg(db *sqlx.DB, id uint64) (*Msg, error) {
	q := `SELECT id, sentence, avasent
	      FROM messages
	      WHERE id=$1`
	m := &Msg{}
	if err := db.Get(m, q, id); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Msg) Update(db *sqlx.DB) error {
	q := `UPDATE messages
	      SET sentence=$1, package=$2
	      WHERE id=$3`
	_, err := db.Exec(q, m.Sentence, m.Package, m.ID)
	return err
}

func (m *Msg) Save(db *sqlx.DB) error {
	q := `INSERT INTO messages
	      (userid, sentence, package, route, sentenceannotated, avasent)
	      VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`
	row := db.QueryRowx(q, m.User.ID, m.Sentence, m.Package, m.Route,
		m.SentenceAnnotated, m.AvaSent)
	if err := row.Scan(&m.ID); err != nil {
		return err
	}
	return nil
}

func (m *Msg) GetLastRoute(db *sqlx.DB) (string, error) {
	var route string
	q := `SELECT route FROM messages
	      WHERE userid=$1 AND avasent IS FALSE
	      ORDER BY createdat DESC`
	err := db.Get(&route, q, m.User.ID)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	return route, nil
}

/*
func (m *Msg) GetLastUserMessage(db *sqlx.DB) error {
	log.Debugln("getting last input")
	q := `SELECT id, sentence FROM messages
	      WHERE userid=$1 AND avasent IS FALSE
	      ORDER BY createdat DESC`
	if err := db.Get(&m.LastUserMsg, q, m.User.ID); err != nil {
		return err
	}
	m.LastUserMsg.SentenceFields = SentenceFields(m.LastUserMsg.Sentence)
	m.LastInput = &tmp
	return nil
}

func (m *Msg) NewResponse() *Resp {
	var uid uint64
	if m.User != nil {
		uid = m.User.ID
	}
	res := &Resp{
		MsgID:  m.ID,
		UserID: uid,
		Route:  m.Route,
	}
	if m.LastResponse != nil {
		res.State = m.LastResponse.State
	}
	return res
}
*/

func (m *Msg) GetLastMsg(db *sqlx.DB) (*Msg, error) {
	log.Debugln("getting last response")
	if m.User == nil {
		return nil, ErrMissingUser
	}
	q := `SELECT id, route, sentence
	      FROM messages
	      WHERE userid=$1 AND avasent IS TRUE
	      ORDER BY createdat DESC`
	row := db.QueryRowx(q, m.User.ID)
	var msg Msg
	log.Debugln("scanning into response")
	err := row.StructScan(&msg)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		log.Println("structscan row ", err)
		return nil, err
	}
	msg.User = m.User
	return &msg, nil
}

func (m *Msg) GetLastState(db *sqlx.DB) error {
	var state []byte
	q := `SELECT state FROM states WHERE pkgname=$1`
	err := db.Get(&state, q, m.Package)
	if err == sql.ErrNoRows {
		log.Errorln("WTF NO STATE FOUND for pkg", m.Package)
		return nil
	}
	if err != nil {
		log.Errorln(err, "WTF", m.Package)
		return err
	}
	err = json.Unmarshal(state, &m.State)
	if err != nil {
		log.Println("unmarshaling state", err)
		return err
	}
	return nil
}

// addContext to a StructuredInput, replacing pronouns with the nouns to which
// they refer. TODO refactor
func addContext(db *sqlx.DB, m *Msg) (*Msg, error) {
	for _, w := range m.StructuredInput.Pronouns() {
		var ctx string
		var err error
		switch nlp.Pronouns[w] {
		case nlp.ObjectI:
			ctx, err = getContextObject(db, m.User,
				m.StructuredInput, "objects")
			if err != nil {
				return m, err
			}
			if ctx == "" {
				return m, nil
			}
			for i, o := range m.StructuredInput.Objects {
				if o != w {
					continue
				}
				m.StructuredInput.Objects[i] = ctx
			}
		case nlp.ActorI:
			ctx, err = getContextObject(db, m.User,
				m.StructuredInput, "actors")
			if err != nil {
				return m, err
			}
			if ctx == "" {
				return m, nil
			}
			for i, o := range m.StructuredInput.Actors {
				if o != w {
					continue
				}
				m.StructuredInput.Actors[i] = ctx
			}
		case nlp.TimeI:
			ctx, err = getContextObject(db, m.User,
				m.StructuredInput, "times")
			if err != nil {
				return m, err
			}
			if ctx == "" {
				return m, nil
			}
			for i, o := range m.StructuredInput.Times {
				if o != w {
					continue
				}
				m.StructuredInput.Times[i] = ctx
			}
		case nlp.PlaceI:
			ctx, err = getContextObject(db, m.User,
				m.StructuredInput, "places")
			if err != nil {
				return m, err
			}
			if ctx == "" {
				return m, nil
			}
			for i, o := range m.StructuredInput.Places {
				if o != w {
					continue
				}
				m.StructuredInput.Places[i] = ctx
			}
		default:
			return m, errors.New("unknown type found for pronoun")
		}
		log.WithFields(log.Fields{
			"fn":  "addContext",
			"ctx": ctx,
		}).Infoln("context found")
	}
	return m, nil
}

func getContextObject(db *sqlx.DB, u *User, si *nlp.StructuredInput,
	datatype string) (string, error) {
	log.Debugln("getting object context")
	var tmp *nlp.StringSlice
	if u == nil {
		return "", ErrMissingUser
	}
	if u != nil {
		q := `SELECT ` + datatype + `
		      FROM inputs
		      WHERE userid=$1 AND array_length(objects, 1) > 0`
		if err := db.Get(&tmp, q, u.ID); err != nil {
			return "", err
		}
	}
	return tmp.Last(), nil
}

func SentenceFields(s string) []string {
	var ret []string
	for _, w := range strings.Fields(s) {
		var end bool
		for _, r := range w {
			switch r {
			case '\'', '"', ',', '.', ':', ';', '!', '?':
				end = true
				ret = append(ret, string(r))
			}
		}
		if end {
			ret = append(ret, strings.ToLower(w[:len(w)-1]))
		} else {
			ret = append(ret, strings.ToLower(w))
		}
	}
	return ret
}
