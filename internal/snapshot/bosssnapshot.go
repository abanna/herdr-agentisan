package snapshot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/abanna/herdr-agentisan/internal/report"
)

// Boss snapshot bounds (NERD-5748), independent of the dashboard contract.
const (
	BossVersion       = 1
	MaxBossWorkers    = 128
	MaxBossFieldBytes = 128
	MaxBossBytes      = 768 << 10
)

// Freshness describes what a boss snapshot source can attest to.
type Freshness string

// The v1 freshness states.
const (
	Fresh       Freshness = "fresh"
	Stale       Freshness = "stale"
	Unavailable Freshness = "unavailable"
)

// BossSource is the time and freshness of one source.
type BossSource struct {
	AsOf  *time.Time `json:"as_of"`
	State Freshness  `json:"state"`
}

// BossSnapshot is the v1 read of the caller's own team.
type BossSnapshot struct {
	V               int         `json:"v"`
	Team            string      `json:"team"`
	At              time.Time   `json:"at"`
	TeamPoll        BossSource  `json:"team_poll"`
	Boss            *BossMember `json:"boss"`
	MissingBossName *string     `json:"missing_boss_name"` // Set when no agent claims the canonical name.
	Groups          []BossGroup `json:"groups"`
	WorkersOmitted  int         `json:"workers_omitted"`
	Board           BossBoard   `json:"board"`
}

// BossRow is the live pane metadata common to the boss and workers.
type BossRow struct {
	PaneID     string     `json:"pane_id"`
	Name       string     `json:"name"`
	Agent      *string    `json:"agent"`
	Status     Status     `json:"status"`
	Item       *string    `json:"item"`
	Stage      *string    `json:"stage"`
	StageAt    *time.Time `json:"stage_at"`
	StageStale bool       `json:"stage_stale"`
	IdleSince  *time.Time `json:"idle_since"`
	Ctx        *int       `json:"ctx"`
}

// BossMember is the canonical boss and its agent's measured start.
type BossMember struct {
	BossRow
	StartedAt *time.Time `json:"started_at"`
}

// BossWorker is a worker with its logical identity when a row binds it.
type BossWorker struct {
	BossRow
	ID         *string `json:"id"`
	Generation *int64  `json:"generation"`
}

// BossGroup holds workers in model order.
type BossGroup struct {
	Name    string       `json:"name"`
	Workers []BossWorker `json:"workers"`
}

// BossBoard is the boss reader's board metadata at one read.
type BossBoard struct {
	BossSource
	Mark   *int64 `json:"mark"`
	Newest *int64 `json:"newest"`
	Unread *int   `json:"unread"`
}

// Validate checks the boss snapshot's value domains.
func (s BossSnapshot) Validate() error {
	if s.V != BossVersion {
		return ErrUnsupportedVersion
	}
	if !boundedBossString(s.Team, 2*MaxBossFieldBytes) || s.At.IsZero() ||
		s.TeamPoll.AsOf == nil || s.TeamPoll.AsOf.IsZero() ||
		(s.TeamPoll.State != Fresh && s.TeamPoll.State != Stale) ||
		s.MissingBossName != nil && (s.Boss != nil || !bossNamePattern.MatchString(*s.MissingBossName)) ||
		s.Groups == nil || len(s.Groups) > MaxBossWorkers || s.WorkersOmitted < 0 || s.WorkersOmitted > math.MaxInt-MaxBossWorkers || !s.Board.valid() {
		return ErrInvalid
	}
	if s.Boss != nil && !s.Boss.valid() {
		return ErrInvalid
	}
	count := 0
	for _, g := range s.Groups {
		count += len(g.Workers)
		if !boundedBossString(g.Name, MaxBossFieldBytes) || g.Workers == nil || count > MaxBossWorkers {
			return ErrInvalid
		}
		for _, w := range g.Workers {
			if !w.valid() || (w.ID == nil) != (w.Generation == nil) ||
				(w.ID != nil && (!boundedBossString(*w.ID, MaxBossFieldBytes) || *w.Generation < 0)) {
				return ErrInvalid
			}
		}
	}
	return nil
}

func boundedBossString(s string, limit int) bool {
	return s != "" && len(s) <= limit && utf8.ValidString(s)
}

func (r BossRow) valid() bool {
	for _, s := range []*string{r.Agent, r.Item} {
		if s != nil && !boundedBossString(*s, MaxBossFieldBytes) {
			return false
		}
	}
	return (r.Stage == nil || report.CheckValue(*r.Stage) == nil) && boundedBossString(r.Name, MaxBossFieldBytes) && boundedBossString(r.PaneID, MaxBossFieldBytes) &&
		r.Status.Valid() && (r.Ctx == nil || *r.Ctx >= 0 && *r.Ctx <= MaxCtx)
}

func (b BossBoard) valid() bool {
	switch b.State {
	case Fresh:
		return b.AsOf != nil && !b.AsOf.IsZero() && b.Mark != nil && *b.Mark >= 0 &&
			b.Newest != nil && *b.Newest >= 0 && b.Unread != nil && *b.Unread >= 0
	case Unavailable:
		return b.AsOf == nil && b.Mark == nil && b.Newest == nil && b.Unread == nil
	default:
		return false
	}
}

func bossObject(keys map[string]*shape) *shape {
	return &shape{keys: keys, required: slices.Sorted(maps.Keys(keys))}
}

var (
	bossNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,7}-boss$`)
	bossScalar      = &shape{nonNull: true}
	bossFields      = map[string]*shape{
		"pane_id": nil, nameKey: nil, "agent": nil, "status": nil, "item": nil, "stage": nil,
		"stage_at": timeShape, "stage_stale": bossScalar, "idle_since": timeShape, "ctx": nil,
	}
	bossSnapshotShape = bossObject(map[string]*shape{
		"v": nil, "team": nil, "at": timeShape,
		"team_poll":         bossObject(map[string]*shape{"as_of": timeShape, "state": nil}),
		"boss":              bossObject(union(bossFields, map[string]*shape{"started_at": timeShape})),
		"missing_boss_name": nil,
		"groups": {elem: bossObject(map[string]*shape{
			nameKey: nil, "workers": {elem: bossObject(union(bossFields, map[string]*shape{"id": nil, "generation": nil}))},
		})},
		"workers_omitted": bossScalar,
		"board": bossObject(map[string]*shape{
			"as_of": timeShape, "state": nil, "mark": nil, "newest": nil, "unread": nil,
		}),
	})
)

// DecodeBoss reads one bounded v1 boss snapshot and checks every key.
func DecodeBoss(r io.Reader) (BossSnapshot, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxBossBytes+1))
	if err != nil {
		return BossSnapshot{}, fmt.Errorf("%w: read: %w", ErrInvalid, err)
	}
	if len(raw) > MaxBossBytes || !utf8.Valid(raw) {
		return BossSnapshot{}, ErrInvalid
	}
	var s BossSnapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return BossSnapshot{}, fmt.Errorf("%w: decode: %w", ErrInvalid, err)
	}
	if s.V != BossVersion {
		return BossSnapshot{}, ErrUnsupportedVersion
	}
	if err := walk(json.NewDecoder(bytes.NewReader(raw)), bossSnapshotShape); err != nil {
		return BossSnapshot{}, err
	}
	if err := s.Validate(); err != nil {
		return BossSnapshot{}, err
	}
	return s, nil
}
