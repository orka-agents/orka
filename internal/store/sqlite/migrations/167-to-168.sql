CREATE TABLE IF NOT EXISTS store_dataset (
 singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
 dataset_id TEXT NOT NULL,
 origin TEXT NOT NULL CHECK(origin IN ('new', 'adopted')),
 bootstrap_complete INTEGER NOT NULL CHECK(bootstrap_complete IN (0, 1))
);
