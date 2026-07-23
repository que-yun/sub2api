-- API-key quota and rate-limit fields participate in request admission.
-- Route configuration sync writes these fields directly, so cached credentials
-- must be invalidated whenever any of them changes.
CREATE OR REPLACE FUNCTION enqueue_api_key_auth_cache_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM enqueue_auth_cache_invalidation(OLD.key);
        RETURN OLD;
    END IF;

    IF OLD.key IS DISTINCT FROM NEW.key
       OR OLD.status IS DISTINCT FROM NEW.status
       OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
       OR OLD.user_id IS DISTINCT FROM NEW.user_id
       OR OLD.group_id IS DISTINCT FROM NEW.group_id
       OR OLD.ip_whitelist IS DISTINCT FROM NEW.ip_whitelist
       OR OLD.ip_blacklist IS DISTINCT FROM NEW.ip_blacklist
       OR OLD.expires_at IS DISTINCT FROM NEW.expires_at
       OR OLD.quota IS DISTINCT FROM NEW.quota
       OR OLD.rate_limit_5h IS DISTINCT FROM NEW.rate_limit_5h
       OR OLD.rate_limit_1d IS DISTINCT FROM NEW.rate_limit_1d
       OR OLD.rate_limit_7d IS DISTINCT FROM NEW.rate_limit_7d THEN
        PERFORM enqueue_auth_cache_invalidation(OLD.key);
        IF NEW.deleted_at IS NULL AND NEW.key IS DISTINCT FROM OLD.key THEN
            PERFORM enqueue_auth_cache_invalidation(NEW.key);
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

-- Flush credentials that may have been cached before this migration. This is
-- a one-time outbox enqueue; usage counters themselves stay runtime-only.
INSERT INTO auth_cache_invalidation_outbox (cache_key)
SELECT encode(sha256(convert_to(key, 'UTF8')), 'hex')
FROM api_keys
WHERE deleted_at IS NULL
  AND key <> ''
  AND (quota > 0 OR rate_limit_5h > 0 OR rate_limit_1d > 0 OR rate_limit_7d > 0);
