'use strict';

const crypto = require('crypto');

// Called at the start of each virtual user's scenario to generate a unique identity.
function generateUser(context, ee, next) {
  const uid = crypto.randomBytes(8).toString('hex');
  context.vars.email    = `load_${uid}@example.com`;
  context.vars.username = `load_${uid}`;
  context.vars.password = 'LoadTest1!';
  return next();
}

module.exports = { generateUser };
