package icloud

import "github.com/google/uuid"

// newUID generates a UID for a new VEVENT, suffixed per RFC 5545's
// recommendation to scope UIDs to a domain.
func newUID() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return id.String() + "@ios-mcp.icloud-calendar", nil
}
