// 003_users_githubid_index.js
//
// ⚠️  THIS TOUCHES A PLATFORM COLLECTION. Read it before running it.
// ⚠️  Shuriken's own database role cannot run this (it is find-only on users).
//     Run it as a privileged account, or skip it — see "Alternative" below.
//
//   mongosh "$MONGODB_URI" --file migrations/003_users_githubid_index.js
//
// WHY
//
// apps/server/src/modules/user/schemas/user.schema.ts declares
//
//   UserSchema.index({ 'githubProfile.githubId': 1 }, { unique: true, sparse: true });
//
// but that index was absent from the development database when Shuriken was
// built — only { 'githubProfile.login': 1 } existed. Shuriken resolves a GitHub
// identity to a platform user on githubProfile.githubId for every recorded
// activity, so without this index each lookup is a collection scan.
//
// ALTERNATIVE
//
// Mongoose autoIndex is not disabled in the platform config, so booting the
// NestJS server once will create this index on its own. If you would rather
// let the application own its schema, skip this file and boot the server.
//
// SAFETY
//
// The index is unique and sparse: documents without githubProfile.githubId are
// not indexed and are unaffected. Creation fails if two users already share a
// github id, which is a data problem you want to know about — the check below
// reports any duplicates before attempting anything.

const users = db.getCollection('users');

print('Checking for duplicate githubProfile.githubId values...');

const duplicates = users
  .aggregate([
    { $match: { 'githubProfile.githubId': { $exists: true, $ne: null } } },
    { $group: { _id: '$githubProfile.githubId', count: { $sum: 1 }, ids: { $push: '$_id' } } },
    { $match: { count: { $gt: 1 } } },
  ])
  .toArray();

if (duplicates.length > 0) {
  print(`\n✗ ${duplicates.length} github id(s) are used by more than one user.`);
  print('  Resolve these before creating a unique index:\n');
  for (const d of duplicates) {
    print(`  githubId ${d._id} -> ${d.ids.map((i) => i.toString()).join(', ')}`);
  }
  print('\nNo index was created.');
} else {
  print('  none found.\n');
  const name = users.createIndex(
    { 'githubProfile.githubId': 1 },
    { unique: true, sparse: true, name: 'githubProfile.githubId_1' }
  );
  print(`+ users.${name}`);
  print('\n003 done.');
}
