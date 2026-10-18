-- The writer and reader logins; the migration Job grants LOGIN once their Secrets exist.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'amp_scores_writer') THEN
        CREATE ROLE amp_scores_writer NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'amp_scores_reader') THEN
        CREATE ROLE amp_scores_reader NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
    END IF;
END
$$;

-- Only the two roles (and the admin that owns the database) may connect.
DO $$
BEGIN
    EXECUTE format('REVOKE ALL ON DATABASE %I FROM PUBLIC', current_database());
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO amp_scores_writer, amp_scores_reader', current_database());
END
$$;

REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO amp_scores_writer, amp_scores_reader;

GRANT SELECT, INSERT, UPDATE, DELETE ON trace_scores, span_scores TO amp_scores_writer;
GRANT SELECT ON trace_scores, span_scores TO amp_scores_reader;
