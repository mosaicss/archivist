import { report } from "./validate.js";
console.log(JSON.stringify(await report(process.argv[2])));
