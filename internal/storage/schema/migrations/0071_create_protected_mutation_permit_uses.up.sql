-- A permit replay identity is consumed with the mutation it authorizes. Keep
-- each identity permanently so clock skew or cleanup races cannot reopen a
-- valid replay.
CREATE TABLE IF NOT EXISTS protected_mutation_permit_uses (
    audience VARCHAR(255) NOT NULL,
    replay_id VARCHAR(255) NOT NULL,
    used_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (audience, replay_id)
);
