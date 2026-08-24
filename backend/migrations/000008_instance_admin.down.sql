-- Drops the Instance Admin tier.
--
-- Destructive in a way the earlier down-migrations are not: every table 000003 through 000007 created
-- holds rows that expire within the hour, so rolling one back costs a retry. This one holds the fact of
-- who administers the instance, and rolling it back leaves an instance whose bootstrap endpoint will
-- answer to the next operator token presented to it, because that endpoint's guard is "are there zero
-- admins". Re-applying 000008 does not bring the rows back.
DROP TABLE instance_admins;
