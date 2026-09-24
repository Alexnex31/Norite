-- Applications first, and the order is load-bearing rather than tidy: DROP TABLE message_tags fails with
-- "cannot drop table message_tags because other objects depend on it" while message_tag_applications
-- still references it. ON DELETE CASCADE governs rows, not DDL. (This comment said the opposite until
-- /code-review read it at M17.)
DROP TABLE message_tag_applications;
DROP TABLE message_tags;
