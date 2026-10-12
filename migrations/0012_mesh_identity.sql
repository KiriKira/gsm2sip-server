-- Physical gateway install that this node's gateway device represents. Several
-- independent nodes can carry the same install; clients use it to verify they
-- are paired to one old phone before merging node histories.
ALTER TABLE gateways ADD COLUMN install_id UUID;
