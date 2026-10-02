CREATE TABLE IF NOT EXISTS shows (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    price_paise BIGINT NOT NULL,
    total_seats INT NOT NULL,
    per_user_limit INT NOT NULL DEFAULT 4,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
) ENGINE = InnoDB;

CREATE TABLE IF NOT EXISTS show_seats (
    show_id BIGINT NOT NULL,
    seat_label VARCHAR(16) COLLATE utf8mb4_bin NOT NULL,
    status ENUM(
        'available',
        'held',
        'confirmed'
    ) NOT NULL DEFAULT 'available',
    reservation_id BIGINT NULL,
    PRIMARY KEY (show_id, seat_label),
    KEY idx_reservation (reservation_id)
) ENGINE = InnoDB;

CREATE TABLE IF NOT EXISTS reservations (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    show_id BIGINT NOT NULL,
    user_id VARCHAR(64) NOT NULL,
    idempotency_key VARCHAR(128) NOT NULL,
    request_hash CHAR(64) NOT NULL,
    seats JSON NOT NULL,
    amount_paise BIGINT NOT NULL,
    status ENUM('confirmed', 'cancelled') NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uq_user_key (user_id, idempotency_key)
) ENGINE = InnoDB;

CREATE TABLE IF NOT EXISTS user_show (
    show_id BIGINT NOT NULL,
    user_id VARCHAR(64) NOT NULL,
    seats_held INT NOT NULL DEFAULT 0,
    PRIMARY KEY (show_id, user_id)
) ENGINE = InnoDB;