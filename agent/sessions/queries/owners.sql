-- name: ClaimOwner :execrows
INSERT INTO owner_claims (platform, user_id, claimed_at, actor) VALUES (?, ?, ?, ?)
ON CONFLICT (platform, user_id) DO NOTHING;

-- name: UnclaimOwner :execrows
DELETE FROM owner_claims WHERE platform = ? AND user_id = ?;

-- name: OwnerClaim :one
SELECT platform, user_id, claimed_at, actor FROM owner_claims WHERE platform = ? AND user_id = ?;

-- name: OwnerClaims :many
SELECT platform, user_id, claimed_at, actor FROM owner_claims ORDER BY platform, user_id;
