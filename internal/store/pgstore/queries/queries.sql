-- name: GetRoom :one
SELECT * FROM rooms WHERE id = $1;

-- name: GetRooms :many
SELECT * FROM rooms;

-- name: CreateRoom :one
INSERT INTO rooms ("theme") VALUES ($1) RETURNING "id";

-- name: GetMessage :one
SELECT * FROM messages WHERE id = $1;

-- name: GetMessagesByRoom :many
SELECT * FROM messages WHERE room_id = $1;

-- name: CreateMessage :one
INSERT INTO messages ("room_id", "message") VALUES ($1, $2) RETURNING "id";

-- name: ReactToMessage :one
UPDATE messages SET reactions_count = reactions_count + 1 WHERE id = $1 RETURNING reactions_count;

-- name: RemoveReactionFromMessage :one
UPDATE messages SET reactions_count = CASE WHEN reactions_count > 0 THEN reactions_count - 1 ELSE 0 END WHERE id = $1 RETURNING reactions_count;

-- name: MarkMessageAsRead :one
UPDATE messages SET answered = true WHERE id = $1 RETURNING answered;

-- name: RemoveMessageFromRoom :one
DELETE FROM messages WHERE id = $1 RETURNING id;