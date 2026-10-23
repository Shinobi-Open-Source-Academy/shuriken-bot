// 003_shuriken_db_user.js
//
// ⚠️  CREATES A ROLE AND A DATABASE USER. Run as a privileged account.
// ⚠️  SET THE PASSWORD VIA THE ENVIRONMENT FIRST. Never commit one.
//
//   SHURIKEN_DB_PASSWORD='...' mongosh "$ADMIN_MONGODB_URI/shuriken" \
//     --file migrations/003_shuriken_db_user.js
//
// Because Shuriken owns this database outright, the role is simply
// readWrite on it. There are no platform collections to carve out and no
// collection-level privileges to maintain, which is the main operational
// simplification of owning the database.
//
// Note what is NOT granted:
//   - no access to any other database, so no consumer's user records
//   - no dropDatabase, dropCollection or dropIndex
//   - no role on `admin`
//
// Earlier revisions of this project granted Shuriken `find` on a platform
// `users` collection to resolve identities. That is gone: MongoDB has no
// field-level privileges, so that grant also exposed bcrypt password hashes.
// Shuriken now stores GitHub identity only and never reads anyone else's data.

const DB_NAME = db.getName();
const ROLE = 'shurikenBot';
const USER = 'shuriken';

if (DB_NAME === 'admin' || DB_NAME === 'local' || DB_NAME === 'config') {
  print(`✗ Refusing to run against the system database "${DB_NAME}".`);
  quit(1);
}

const password = process.env.SHURIKEN_DB_PASSWORD;
if (!password) {
  print('✗ SHURIKEN_DB_PASSWORD is not set. Refusing to create a user without one.');
  quit(1);
}

const collections = [
  'shuriken_activities',
  'shuriken_review_findings',
  'shuriken_review_runs',
  'shuriken_deliveries',
  'shuriken_outbox',
];

// Explicit per-collection privileges rather than the built-in readWrite role.
// readWrite would also grant dropCollection; Shuriken never needs to drop
// anything, and the service performs no DDL at startup.
const actions = [
  'find',
  'insert',
  'update',
  'createIndex',
  'listIndexes',
  'listCollections',
  'createCollection',
];

const privileges = collections.map((collection) => ({
  resource: { db: DB_NAME, collection },
  actions,
}));

const existingRole = db.getRole(ROLE);
if (existingRole) {
  db.updateRole(ROLE, { privileges, roles: [] });
  print(`= updated role ${ROLE}`);
} else {
  db.createRole({ role: ROLE, privileges, roles: [] });
  print(`+ created role ${ROLE}`);
}

const existingUser = db.getUser(USER);
if (existingUser) {
  db.updateUser(USER, { roles: [{ role: ROLE, db: DB_NAME }] });
  print(`= updated user ${USER} (password unchanged)`);
} else {
  db.createUser({ user: USER, pwd: password, roles: [{ role: ROLE, db: DB_NAME }] });
  print(`+ created user ${USER}`);
}

print(`\nGranted on ${DB_NAME} only:`);
for (const c of collections) {
  print(`  ${c.padEnd(26)} ${actions.join(', ')}`);
}

print('\nSet SHURIKEN_MONGODB_URI to:');
print(`  mongodb://${USER}:<password>@<host>:27017/${DB_NAME}?authSource=${DB_NAME}`);
print('\n003 done.');
