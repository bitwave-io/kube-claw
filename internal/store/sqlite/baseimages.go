package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/traego/kube-claw/internal/store"
)

// CreateBaseImage registers or replaces a named base image.
func (t *tx) CreateBaseImage(b store.BaseImage) error {
	if b.CreatedAt == "" {
		b.CreatedAt = store.NowRFC3339()
	}
	_, err := t.tx.Exec(
		`INSERT INTO base_images (name, image, description, created_at) VALUES (?,?,?,?)
		 ON CONFLICT(name) DO UPDATE SET image=excluded.image, description=excluded.description`,
		b.Name, b.Image, b.Description, b.CreatedAt)
	if err != nil {
		return fmt.Errorf("create base image: %w", err)
	}
	return nil
}

// GetBaseImage returns a base image by name.
func (t *tx) GetBaseImage(name string) (store.BaseImage, error) {
	var b store.BaseImage
	var desc sql.NullString
	err := t.tx.QueryRow(
		`SELECT name, image, description, created_at FROM base_images WHERE name=?`, name,
	).Scan(&b.Name, &b.Image, &desc, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.BaseImage{}, store.ErrNotFound
	}
	if err != nil {
		return store.BaseImage{}, err
	}
	b.Description = desc.String
	return b, nil
}

// ListBaseImages returns all registered base images.
func (t *tx) ListBaseImages() ([]store.BaseImage, error) {
	rows, err := t.tx.Query(`SELECT name, image, description, created_at FROM base_images ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.BaseImage
	for rows.Next() {
		var b store.BaseImage
		var desc sql.NullString
		if err := rows.Scan(&b.Name, &b.Image, &desc, &b.CreatedAt); err != nil {
			return nil, err
		}
		b.Description = desc.String
		out = append(out, b)
	}
	return out, rows.Err()
}
