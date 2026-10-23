// 001_shuriken_collections.js
//
// Creates Shuriken's collections. Safe to re-run: each creation is guarded on
// the existing collection list.
//
//   mongosh "$SHURIKEN_MONGODB_URI" --file migrations/001_shuriken_collections.js
//
// This runs against SHURIKEN'S OWN DATABASE (default name: `shuriken`). It
// must not be pointed at a consumer's database. Shuriken holds no credential
// for anyone else's data and writes nothing outside these collections; see
// docs/adr/0005-shuriken-owns-its-database.md.

const collections = [
  'shuriken_activities',
  'shuriken_review_findings',
  'shuriken_review_runs',
  'shuriken_deliveries',
  // The outbound delivery queue. Because a signed webhook is the only way a
  // consumer learns anything, events are made durable here before delivery is
  // attempted, so a crash costs a retry rather than an event.
  'shuriken_outbox',
];

print(`database: ${db.getName()}`);
if (db.getCollectionNames().some((n) => !n.startsWith('shuriken_'))) {
  print('');
  print('⚠️  This database contains collections that are not Shuriken\'s.');
  print('⚠️  Check SHURIKEN_MONGODB_URI points at Shuriken\'s own database');
  print('⚠️  before continuing. Nothing has been created.');
  quit(1);
}

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
