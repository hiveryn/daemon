-- Conversation input is delivered synchronously: a receipt records the
-- outcome of that attempt and, separately, provider acceptance.
ALTER TABLE conversation_submissions ADD COLUMN error_code TEXT NOT NULL DEFAULT '';
ALTER TABLE conversation_submissions ADD COLUMN accepted_at TEXT NOT NULL DEFAULT '';
ALTER TABLE conversation_submissions ADD COLUMN acceptance TEXT NOT NULL DEFAULT '';
-- Cutover. User input still waiting in the former Hiveryn queue was never
-- handed to the provider: it is settled as not sent (text kept), never
-- dispatched later. Internal chat kickoff/notification entries stay queued.
UPDATE conversation_submissions
   SET status='rejected', error_code='QUEUED_BEFORE_CUTOVER',
       error='Not sent: it was waiting in the former Hiveryn queue when synchronous delivery replaced it. Send it again if it is still wanted.',
       updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE origin='user' AND status='queued';
-- A former "started" entry whose input appeared in the native conversation
-- was accepted; its turn outcome is still settled from native evidence.
UPDATE conversation_submissions
   SET status='running', accepted_at=CASE WHEN handed_at<>'' THEN handed_at ELSE updated_at END, acceptance='native_record'
 WHERE status='started' AND native_turn_id<>'';
-- Any other "started" entry was reserved for a hand-off whose acceptance was
-- never established; it is reconciled from native evidence, never resent.
UPDATE conversation_submissions
   SET status='unknown', error_code='DELIVERY_UNKNOWN'
 WHERE status='started';
-- Settled entries with native turn evidence were accepted. The former
-- records kept no acceptance time; the hand-off (or admission) time stands in.
UPDATE conversation_submissions
   SET accepted_at=CASE WHEN handed_at<>'' THEN handed_at ELSE created_at END, acceptance='native_record'
 WHERE accepted_at='' AND (status IN ('completed','blocked') OR (status IN ('failed','uncertain') AND native_turn_id<>''));
