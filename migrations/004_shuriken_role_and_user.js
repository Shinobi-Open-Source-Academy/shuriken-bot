// 004_shuriken_role_and_user.js
//
// ⚠️  CREATES A ROLE AND A DATABASE USER. Run as a privileged account.
// ⚠️  SET THE PASSWORD BELOW FIRST. Do not commit a real password.
//
//   SHURIKEN_DB_PASSWORD='...' mongosh "$MONGODB_URI" --file migrations/004_shuriken_role_and_user.js
//
// Grants Shuriken read-write access to its own four collections and find-only
// access to the two platform collections it needs for identity mapping. There
// is no remove privilege anywhere, and no access to sessions, posts,
// communities, blog content or anything else.
//
// MongoDB grants per database, but collection-level privileges are expressible
// through a custom role, which is what this builds.

const DB_NAME = db.getName();
const ROLE = 'shurikenBot';
const USER = 'shuriken';

const password = process.env.SHURIKEN_DB_PASSWORD;
if (!password) {
  print('✗ SHURIKEN_DB_PASSWORD is not set. Refusing to create a user without one.');
  quit(1);
}

// Shuriken's own collections: read, write and index management.
const ownedCollections = [
  'shuriken_activities',
  'shuriken_review_findings',
  'shuriken_review_runs',
  'shuriken_deliveries',
];

const ownedActions = [
  'find',
  'insert',
  'update',
  'createIndex',
  'listIndexes',
  'listCollections',
  'createCollection',
];

// Platform collections: find only. Shuriken never writes to these and never
// creates platform accounts.
const readOnlyCollections = ['users', 'projects'];

const privileges = [
  ...ownedCollections.map((collection) => ({
    resource: { db: DB_NAME, collection },
    actions: ownedActions,
  })),
  ...readOnlyCollections.map((collection) => ({
    resource: { db: DB_NAME, collection },
    actions: ['find'],
  })),
];

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

print('\nGranted:');
for (const p of privileges) {
  print(`  ${p.resource.collection.padEnd(26)} ${p.actions.join(', ')}`);
}

print(`\nSet SHURIKEN_MONGODB_URI to:`);
print(`  mongodb://${USER}:<password>@<host>:27017/${DB_NAME}?authSource=${DB_NAME}`);
print('\n004 done.');
