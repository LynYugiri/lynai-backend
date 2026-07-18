CREATE TABLE community_profiles (
    user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    bio TEXT NOT NULL DEFAULT '',
    pinned_post_id BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE community_posts (
    id BIGINT PRIMARY KEY,
    author_id BIGINT NOT NULL REFERENCES users(id),
    title VARCHAR(120) NOT NULL DEFAULT '',
    content TEXT NOT NULL,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_community_posts_feed ON community_posts (created_at DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_community_posts_author ON community_posts (author_id, created_at DESC, id DESC) WHERE deleted_at IS NULL;

ALTER TABLE community_profiles
    ADD CONSTRAINT fk_community_profiles_pinned_post
    FOREIGN KEY (pinned_post_id) REFERENCES community_posts(id) ON DELETE SET NULL;

CREATE TABLE community_comments (
    id BIGINT PRIMARY KEY,
    post_id BIGINT NOT NULL REFERENCES community_posts(id) ON DELETE CASCADE,
    author_id BIGINT NOT NULL REFERENCES users(id),
    content TEXT NOT NULL,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_community_comments_post ON community_comments (post_id, created_at ASC, id ASC) WHERE deleted_at IS NULL;

CREATE TABLE community_likes (
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    post_id BIGINT NOT NULL REFERENCES community_posts(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, post_id)
);
CREATE INDEX idx_community_likes_post ON community_likes (post_id);

CREATE TABLE community_favorites (
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    post_id BIGINT NOT NULL REFERENCES community_posts(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, post_id)
);
CREATE INDEX idx_community_favorites_user ON community_favorites (user_id, created_at DESC);

CREATE TABLE community_media (
    id BIGINT PRIMARY KEY,
    owner_user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    sha256 VARCHAR(64) NOT NULL,
    path TEXT NOT NULL,
    media_type VARCHAR(32) NOT NULL,
    size BIGINT NOT NULL,
    width BIGINT NOT NULL,
    height BIGINT NOT NULL,
    attached_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_community_media_owner ON community_media (owner_user_id, created_at DESC);
CREATE INDEX idx_community_media_sha256 ON community_media (sha256);

CREATE TABLE community_post_media (
    post_id BIGINT NOT NULL REFERENCES community_posts(id) ON DELETE CASCADE,
    media_id BIGINT NOT NULL REFERENCES community_media(id),
    sort_order BIGINT NOT NULL,
    PRIMARY KEY (post_id, sort_order),
    UNIQUE (media_id)
);
CREATE INDEX idx_community_post_media_media ON community_post_media (media_id);

CREATE TABLE community_audit_records (
    id BIGINT PRIMARY KEY,
    actor_id BIGINT NOT NULL REFERENCES users(id),
    action VARCHAR(64) NOT NULL,
    target_type VARCHAR(32) NOT NULL,
    target_id BIGINT NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_community_audit_created ON community_audit_records (created_at DESC, id DESC);
CREATE INDEX idx_community_audit_target ON community_audit_records (target_type, target_id);
