-- Dedup asks "has this incident already seen this fingerprint?" once per
-- arriving alert. With only idx_alerts_incident to work with, that scanned
-- every alert already attached to the incident, so ingesting a cascade was
-- quadratic: the thousandth alert of a group scanned nine hundred and
-- ninety-nine rows.
--
-- A cascade is precisely when ingest must not fall over, so this is the one
-- index whose absence matters most.

CREATE INDEX idx_alerts_incident_fingerprint ON alerts(incident_id, fingerprint);
