// 001_shuriken_collections.js
//
// Creates the four collections Shuriken owns. Safe to re-run: each creation is
// guarded on the existing collection list.
//
//   mongosh "$MONGODB_URI" --file migrations/001_shuriken_collections.js
//
// This writes nothing to any platform collection.

const collections = [
  'shuriken_activities',
  'shuriken_review_findings',
  'shuriken_review_runs',
  'shuriken_deliveries',
];

const existing = new Set(db.getCollectionNames());

for (const name of collections) {
  if (existing.has(name)) {
    print(`= ${name} already exists`);
    continue;
  }
  db.createCollection(name);
  print(`+ created ${name}`);
}

print('\n001 done.');
