'use strict';

// Re-export the install entrypoint so programmatic callers can use:
//   const { install } = require('@mosaic-finance/archivist');
//   install();
//
// The typical usage path is `npx -y @mosaic-finance/archivist install`,
// which invokes bin/archivist-install.js directly via the `bin` field in
// package.json.

module.exports = {
  install() {
    require('./bin/archivist-install.js');
  },
};
