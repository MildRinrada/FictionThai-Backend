package shelves

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/internal/profiles"
	"github.com/fictionthai/fictionthai/backend/internal/users"
)

// ErrNotFound covers every reason a shelf or an owner cannot be reached. The
// service translates it into the single 404 all of them share.
var ErrNotFound = errors.New("shelf not found")

// Repository is the only place that reads or writes `shelves` and
// `shelf_items`. It never touches `bookmarks`.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// argList builds a positional-parameter list, so every value is bound rather
// than interpolated (docs/11 §15).
type argList struct {
	args []any
}

func (a *argList) add(value any) string {
	a.args = append(a.args, value)
	return "$" + strconv.Itoa(len(a.args))
}

// publicItemSQL is the predicate for "a fiction a PUBLIC shelf may show",
// against alias `n`.
//
// novels.ListedSQL is the shared guest-tier BROWSE predicate, used rather than
// copied: a shelf that decided this by its own rule would eventually publish a
// private draft, which is the whole failure mode docs/11 §31 names. It is the
// guest constant, not a ...For variant, because this listing is
// viewer-INDEPENDENT - the same bytes for everyone, so one cached response
// serves the whole page (docs/14 §7).
//
// It is ListedSQL and not ReadableSQL, which is the fix for a real leak: a
// public shelf is a browse surface, and readable admits `unlisted` work. A
// writer who chose ลิงก์ลับ - told, in the studio, that "เรื่องไม่ขึ้นหน้ารวม" -
// had their fiction listed to every guest the moment anyone shelved it.
// Readable answers "may this person open it"; a shelf is asking the different
// question "may this be advertised".
//
// The rating clause is the second half of the same idea. A public shelf is a
// browse surface, and explicit work is never listed on one however the reader
// has set their preferences (§13B, AgeRating.NeverListed) - it stays reachable
// by link and from its author's own page, which is where it belongs.
func publicItemSQL(args *argList) string {
	return `((` + novels.ListedSQL + `)
		AND n.age_rating <> ` + args.add(string(novels.RatingExplicit)) + `)`
}

// ownerItemSQL is the predicate for the owner's OWN listing, against `n`:
// anything this person may open, plus their own work.
//
// It composes the same shared predicate with the viewer bound, exactly as the
// library's shelf does, so a members-only fiction they can still open does not
// vanish from the manager while its link keeps working (§13C). The owner clause
// covers a writer shelving their own unpublished work. Neither clause can widen
// anyone else's view - userID is always the authenticated caller.
func ownerItemSQL(args *argList, userID uuid.UUID) string {
	viewer := args.add(novels.ViewerValue(userID)) + "::uuid"
	return `((` + novels.ReadableSQLFor(viewer) + `)
		OR (n.deleted_at IS NULL AND n.author_id = ` + viewer + `))`
}

// visibleAccountSQL is "an account that has a public profile", against `u` -
// the same rule the public profile read applies, for the same reason: a shelf
// listing must not outlive the page it belongs to.
const visibleAccountSQL = `
	u.deleted_at IS NULL AND u.status NOT IN ('deleted', 'banned')`

// ResolveOwner turns a `/users/:user` reference into an account id, or
// ErrNotFound for anything a stranger may not see.
func (r *Repository) ResolveOwner(ctx context.Context, ref profiles.Ref) (uuid.UUID, error) {
	var (
		where string
		arg   any
	)
	if ref.ByUsername() {
		// username is CITEXT, so this is the same case-insensitive comparison
		// registration used (docs/10 §7).
		where = `u.username = $1`
		arg = users.NormalizeUsername(ref.Username)
	} else {
		where = `u.id = $1`
		arg = ref.ID
	}

	var id uuid.UUID
	err := r.db.QueryRowContext(ctx,
		`SELECT u.id FROM users u WHERE `+where+` AND `+visibleAccountSQL, arg).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve shelf owner: %w", err)
	}
	return id, nil
}

// shelfColumns is shared by every SELECT so a new column cannot be added to one
// query and forgotten in another. The bare form is used by INSERT/UPDATE
// RETURNING, where there is no FROM clause and therefore no `s` alias - the
// same pair the novels repository keeps for the same reason.
const shelfColumns = `
	s.id, s.user_id, s.name, s.note, s.is_public, s.position,
	s.created_at, s.updated_at`

const shelfColumnsBare = `
	id, user_id, name, note, is_public, position, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scanShelf(row scanner) (*Shelf, error) {
	var shelf Shelf
	err := row.Scan(&shelf.ID, &shelf.UserID, &shelf.Name, &shelf.Note,
		&shelf.IsPublic, &shelf.Position, &shelf.CreatedAt, &shelf.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan shelf: %w", err)
	}
	return &shelf, nil
}

// List returns one person's shelves in their chosen order.
//
// publicOnly is what separates the two callers: the stranger's listing takes
// only the shelves whose owner opted in, and counts only the items a stranger
// may see; the owner's listing takes everything. Both counts ride the same
// correlated subquery so a shelf's number can never disagree with its rows.
func (r *Repository) List(
	ctx context.Context, ownerID uuid.UUID, publicOnly bool,
) ([]Shelf, error) {
	args := &argList{}
	owner := args.add(ownerID)

	// Only the predicate that will actually appear may be BUILT: each one binds
	// its own parameters as it goes, and a placeholder that ends up unreferenced
	// leaves PostgreSQL unable to type it.
	var itemFilter, where string
	if publicOnly {
		itemFilter, where = publicItemSQL(args), ` AND s.is_public`
	} else {
		itemFilter = ownerItemSQL(args, ownerID)
	}

	query := `SELECT ` + shelfColumns + `,
		(
			SELECT count(*) FROM shelf_items si
			JOIN novels n ON n.id = si.novel_id
			WHERE si.shelf_id = s.id AND ` + itemFilter + `
		) AS item_count
		FROM shelves s
		WHERE s.user_id = ` + owner + where + `
		ORDER BY s.position, s.created_at, s.id`

	rows, err := r.db.QueryContext(ctx, query, args.args...)
	if err != nil {
		return nil, fmt.Errorf("list shelves: %w", err)
	}
	defer func() { _ = rows.Close() }()

	list := []Shelf{}
	for rows.Next() {
		var shelf Shelf
		if err := rows.Scan(&shelf.ID, &shelf.UserID, &shelf.Name, &shelf.Note,
			&shelf.IsPublic, &shelf.Position, &shelf.CreatedAt, &shelf.UpdatedAt,
			&shelf.ItemCount); err != nil {
			return nil, fmt.Errorf("scan shelf: %w", err)
		}
		list = append(list, shelf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list shelves: %w", err)
	}
	return list, nil
}

// ItemRow is one shelf entry before its fiction card is attached.
type ItemRow struct {
	ShelfID uuid.UUID
	NovelID uuid.UUID
	Note    *string
	AddedAt sql.NullTime
}

// Items loads the first ItemPreviewLimit entries of EVERY given shelf in one
// query, keyed by shelf.
//
// One query rather than one per shelf: the window function ranks each shelf's
// rows independently, so a person with twelve shelves costs the same as a person
// with one (docs/07 §67). The same visibility predicate the counts used is
// applied here, which is what keeps count and contents consistent.
func (r *Repository) Items(
	ctx context.Context, shelfIDs []uuid.UUID, viewerID uuid.UUID, publicOnly bool,
) (map[uuid.UUID][]ItemRow, error) {
	if len(shelfIDs) == 0 {
		return map[uuid.UUID][]ItemRow{}, nil
	}

	args := &argList{}
	placeholders := make([]string, 0, len(shelfIDs))
	for _, id := range shelfIDs {
		placeholders = append(placeholders, args.add(id))
	}

	var itemFilter string
	if publicOnly {
		itemFilter = publicItemSQL(args)
	} else {
		itemFilter = ownerItemSQL(args, viewerID)
	}

	query := `
		SELECT shelf_id, novel_id, note, added_at FROM (
			SELECT si.shelf_id, si.novel_id, si.note, si.added_at,
				row_number() OVER (
					PARTITION BY si.shelf_id
					ORDER BY si.added_at DESC, si.novel_id DESC
				) AS item_rank
			FROM shelf_items si
			JOIN novels n ON n.id = si.novel_id
			WHERE si.shelf_id IN (` + strings.Join(placeholders, ", ") + `)
			  AND ` + itemFilter + `
		) ranked
		WHERE item_rank <= ` + args.add(ItemPreviewLimit) + `
		ORDER BY shelf_id, item_rank`

	rows, err := r.db.QueryContext(ctx, query, args.args...)
	if err != nil {
		return nil, fmt.Errorf("list shelf items: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byShelf := map[uuid.UUID][]ItemRow{}
	for rows.Next() {
		var row ItemRow
		if err := rows.Scan(&row.ShelfID, &row.NovelID, &row.Note, &row.AddedAt); err != nil {
			return nil, fmt.Errorf("scan shelf item: %w", err)
		}
		byShelf[row.ShelfID] = append(byShelf[row.ShelfID], row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list shelf items: %w", err)
	}
	return byShelf, nil
}

// Find loads one shelf by id, with no visibility filter at all. The service
// decides who may see it - a stranger asking after a private shelf gets the
// same 404 an unknown id gets (docs/11 §3.4).
func (r *Repository) Find(ctx context.Context, shelfID uuid.UUID) (*Shelf, error) {
	return scanShelf(r.db.QueryRowContext(ctx,
		`SELECT `+shelfColumns+` FROM shelves s WHERE s.id = $1`, shelfID))
}

// CountForOwner reports how many shelves a person has, for the create cap.
func (r *Repository) CountForOwner(ctx context.Context, userID uuid.UUID) (int, error) {
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM shelves WHERE user_id = $1`, userID).Scan(&total); err != nil {
		return 0, fmt.Errorf("count shelves: %w", err)
	}
	return total, nil
}

// CountItems reports how many fictions a shelf holds, unfiltered - the cap is
// about what the owner put there, not about what a viewer can see.
func (r *Repository) CountItems(ctx context.Context, shelfID uuid.UUID) (int, error) {
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM shelf_items WHERE shelf_id = $1`, shelfID).Scan(&total); err != nil {
		return 0, fmt.Errorf("count shelf items: %w", err)
	}
	return total, nil
}

// Create adds a shelf at the end of the owner's order.
//
// The position is computed inside the INSERT so two concurrent creates cannot
// read the same "next" value (the characters precedent).
func (r *Repository) Create(
	ctx context.Context, userID uuid.UUID, name string, note *string, isPublic bool,
) (*Shelf, error) {
	return scanShelf(r.db.QueryRowContext(ctx, `
		INSERT INTO shelves (user_id, name, note, is_public, position)
		VALUES ($1, $2, $3, $4,
			COALESCE((SELECT MAX(position) + 1 FROM shelves WHERE user_id = $1), 0))
		RETURNING `+shelfColumnsBare, userID, name, note, isPublic))
}

// Update writes the fields the caller actually supplied.
//
// Every value arrives as a pair - the new value and whether to apply it - so an
// absent field keeps what is stored and an explicit empty string clears it,
// without the repository needing to know which case the HTTP layer saw
// (docs/09 §3).
func (r *Repository) Update(
	ctx context.Context, shelfID uuid.UUID, edit *Edit,
) (*Shelf, error) {
	return scanShelf(r.db.QueryRowContext(ctx, `
		UPDATE shelves SET
			name       = COALESCE($2::text, name),
			note       = CASE WHEN $3::text IS NULL THEN note
			                  WHEN $3 = '' THEN NULL ELSE $3 END,
			is_public  = COALESCE($4::boolean, is_public),
			position   = COALESCE($5::int, position),
			updated_at = now()
		WHERE id = $1
		RETURNING `+shelfColumnsBare,
		shelfID, edit.Name, edit.Note, edit.IsPublic, edit.Position))
}

// Delete removes one shelf. Its items go with it (ON DELETE CASCADE); the
// FICTIONS are untouched, and so is the owner's bookmark of any of them.
func (r *Repository) Delete(ctx context.Context, shelfID uuid.UUID) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM shelves WHERE id = $1`, shelfID)
	if err != nil {
		return fmt.Errorf("delete shelf: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete shelf: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// AddItem places a fiction on a shelf.
//
// ON CONFLICT DO UPDATE makes a repeat idempotent AND lets a second call edit
// the note, which is what a person means when they add something they already
// added. added_at is kept - re-adding is not re-shelving.
func (r *Repository) AddItem(
	ctx context.Context, shelfID, novelID uuid.UUID, note *string,
) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO shelf_items (shelf_id, novel_id, note)
		VALUES ($1, $2, $3)
		ON CONFLICT (shelf_id, novel_id) DO UPDATE SET note = EXCLUDED.note`,
		shelfID, novelID, note)
	if err != nil {
		return fmt.Errorf("add shelf item: %w", err)
	}
	return nil
}

// RemoveItem takes a fiction off a shelf. Removing what is not there is a
// no-op, for the same reason removing a bookmark is: taking something back must
// always work, including for a fiction that has since gone private
// (docs/01 §11).
func (r *Repository) RemoveItem(ctx context.Context, shelfID, novelID uuid.UUID) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM shelf_items WHERE shelf_id = $1 AND novel_id = $2`,
		shelfID, novelID); err != nil {
		return fmt.Errorf("remove shelf item: %w", err)
	}
	return nil
}
