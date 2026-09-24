-- Applications first, though the cascade would take them: dropping the parent while a child table still
-- references it is a dependency Postgres resolves for us, and being explicit costs nothing and says the
-- order out loud.
DROP TABLE message_tag_applications;
DROP TABLE message_tags;
