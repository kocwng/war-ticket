-- Skema bersama untuk semua implementasi.

CREATE TABLE IF NOT EXISTS events (
    id          BIGINT PRIMARY KEY,
    name        TEXT        NOT NULL,
    capacity    INTEGER     NOT NULL CHECK (capacity >= 0),
    -- Sengaja tidak pakai CHECK (stock >= 0) supaya kebocoran strategi naive
    -- kelihatan sebagai stok minus. Di production, constraint ini wajib ada.
    stock       INTEGER     NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tickets (
    id          UUID        PRIMARY KEY,
    event_id    BIGINT      NOT NULL REFERENCES events (id),
    user_id     TEXT        NOT NULL,
    strategy    TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Satu user satu tiket per event. Constraint ini juga yang bikin beli ulang
    -- aman, karena user tidak mungkin dapat tiket kedua.
    UNIQUE (event_id, user_id)
);

INSERT INTO events (id, name, capacity, stock)
VALUES (1, 'Konser Senja di GBK', 500, 500)
ON CONFLICT (id) DO NOTHING;
