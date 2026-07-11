package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/traego/kube-claw/internal/store"
)

const connectorCols = `id, name, callback_url, api_key_hash, signing_secret, agent_ns, agent_name, disabled, created_at`

// CreateConnector registers an external connector.
func (t *tx) CreateConnector(c store.Connector) error {
	if c.CreatedAt == "" {
		c.CreatedAt = store.NowRFC3339()
	}
	disabled := 0
	if c.Disabled {
		disabled = 1
	}
	_, err := t.tx.Exec(
		`INSERT INTO connectors (`+connectorCols+`) VALUES (?,?,?,?,?,?,?,?,?)`,
		c.ID, c.Name, c.CallbackURL, c.APIKeyHash, c.SigningSecret,
		c.AgentNamespace, c.AgentName, disabled, c.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create connector: %w", err)
	}
	return nil
}

func scanConnector(s interface{ Scan(...any) error }) (store.Connector, error) {
	var c store.Connector
	var disabled int
	err := s.Scan(&c.ID, &c.Name, &c.CallbackURL, &c.APIKeyHash, &c.SigningSecret,
		&c.AgentNamespace, &c.AgentName, &disabled, &c.CreatedAt)
	if err != nil {
		return c, err
	}
	c.Disabled = disabled != 0
	return c, nil
}

// GetConnector returns a connector by id.
func (t *tx) GetConnector(id string) (store.Connector, error) {
	c, err := scanConnector(t.tx.QueryRow(`SELECT `+connectorCols+` FROM connectors WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Connector{}, store.ErrNotFound
	}
	return c, err
}

// GetConnectorByKeyHash returns the connector owning an API key hash.
func (t *tx) GetConnectorByKeyHash(hash string) (store.Connector, error) {
	c, err := scanConnector(t.tx.QueryRow(`SELECT `+connectorCols+` FROM connectors WHERE api_key_hash=?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Connector{}, store.ErrNotFound
	}
	return c, err
}

// ListConnectors returns all connectors, newest first.
func (t *tx) ListConnectors() ([]store.Connector, error) {
	rows, err := t.tx.Query(`SELECT ` + connectorCols + ` FROM connectors ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Connector
	for rows.Next() {
		c, err := scanConnector(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetConnectorKeyHash replaces a connector's API key hash (rotation).
func (t *tx) SetConnectorKeyHash(id, hash string) error {
	res, err := t.tx.Exec(`UPDATE connectors SET api_key_hash=? WHERE id=?`, hash, id)
	if err != nil {
		return fmt.Errorf("rotate connector key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteConnector removes a connector.
func (t *tx) DeleteConnector(id string) error {
	_, err := t.tx.Exec(`DELETE FROM connectors WHERE id=?`, id)
	return err
}
