-- This file is appended to pg_restore's SQL output and executed by one
-- psql --single-transaction invocation. Keep each statement compatible with
-- server migrations from 0001 onward; full-table guards support older dumps.

DO $$
BEGIN
    -- Identity tables exist in migration 0001. The remaining sections are
    -- conditional so backups taken before those migrations stay restorable.
    IF to_regclass('public.sessions') IS NOT NULL THEN
        UPDATE public.sessions
        SET access_expires_at = '-infinity'::timestamptz,
            refresh_expires_at = '-infinity'::timestamptz;
    END IF;

    IF to_regclass('public.pairing_codes') IS NOT NULL THEN
        UPDATE public.pairing_codes
        SET consumed_at = COALESCE(consumed_at, clock_timestamp());
    END IF;

    IF to_regclass('public.refresh_recoveries') IS NOT NULL THEN
        DELETE FROM public.refresh_recoveries;
    END IF;

    IF to_regclass('public.commands') IS NOT NULL THEN
        UPDATE public.commands
        SET status = 'unknown', updated_at = clock_timestamp()
        WHERE status IN ('queued', 'accepted_by_gateway', 'dispatching');
    END IF;

    IF to_regclass('public.messages') IS NOT NULL THEN
        UPDATE public.messages
        SET status = 'unknown', updated_at = clock_timestamp()
        WHERE direction = 'outbound'
          AND status IN ('queued', 'accepted_by_gateway', 'dispatching');
    END IF;

    IF to_regclass('public.message_parts') IS NOT NULL THEN
        UPDATE public.message_parts
        SET state = 'unknown', updated_at = clock_timestamp()
        WHERE state = 'dispatching';
    END IF;

    IF to_regclass('public.outbox') IS NOT NULL THEN
        UPDATE public.outbox
        SET processed_at = clock_timestamp()
        WHERE processed_at IS NULL;
    END IF;

    IF to_regclass('public.device_sip_credentials') IS NOT NULL THEN
        UPDATE public.device_sip_credentials
        SET replay_expires_at = '-infinity'::timestamptz;
    END IF;

    IF to_regclass('public.devices') IS NOT NULL THEN
        UPDATE public.devices SET state = 'revoked' WHERE state = 'active';
    END IF;

    IF to_regclass('public.sip_endpoint_bindings') IS NOT NULL THEN
        UPDATE public.sip_endpoint_bindings
        SET state = 'revoked', updated_at = clock_timestamp()
        WHERE state = 'active';
    END IF;

    IF to_regclass('public.ps_auths') IS NOT NULL THEN
        DELETE FROM public.ps_auths;
    END IF;
    IF to_regclass('public.ps_endpoints') IS NOT NULL THEN
        DELETE FROM public.ps_endpoints;
    END IF;
    IF to_regclass('public.ps_aors') IS NOT NULL THEN
        DELETE FROM public.ps_aors;
    END IF;

    IF to_regclass('public.call_intents') IS NOT NULL THEN
        UPDATE public.call_intents
        SET state = 'cancelled', updated_at = clock_timestamp()
        WHERE state = 'reserved';
    END IF;

    IF to_regclass('public.call_sessions') IS NOT NULL THEN
        UPDATE public.call_sessions
        SET state = 'unknown',
            reason = 'backup_restore_requires_reconciliation',
            state_revision = state_revision + 1,
            wake_nonce = NULL,
            updated_at = clock_timestamp()
        WHERE state <> 'ended';
    END IF;

    IF to_regclass('public.call_participants') IS NOT NULL THEN
        UPDATE public.call_participants
        SET wake_nonce = 'backup-restore-revoked-' || call_id::text || '-' || client_device_id::text,
            updated_at = clock_timestamp()
        WHERE state <> 'ended';
    END IF;
END
$$;
