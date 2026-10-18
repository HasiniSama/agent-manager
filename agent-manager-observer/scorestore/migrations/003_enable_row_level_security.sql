ALTER TABLE trace_scores ENABLE ROW LEVEL SECURITY;
ALTER TABLE span_scores ENABLE ROW LEVEL SECURITY;

-- The reader sees only the org its transaction set; an unset or empty amp.org_id matches no row.
CREATE POLICY reader_org ON trace_scores FOR SELECT TO amp_scores_reader
    USING (org_id = NULLIF(current_setting('amp.org_id', true), ''));
CREATE POLICY reader_org ON span_scores FOR SELECT TO amp_scores_reader
    USING (org_id = NULLIF(current_setting('amp.org_id', true), ''));

-- The writer writes and deletes for every org; it checks identity before each write.
CREATE POLICY writer_all ON trace_scores FOR ALL TO amp_scores_writer
    USING (true) WITH CHECK (true);
CREATE POLICY writer_all ON span_scores FOR ALL TO amp_scores_writer
    USING (true) WITH CHECK (true);
