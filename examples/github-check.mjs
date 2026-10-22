import { GitHub, repository } from './github-publisher.mjs';
import { scrub } from './adapter-runtime.mjs';

try {
  const github = new GitHub();
  const user = await github.api('/user');
  console.log(`GitHub authorization: @${user.login}`);
  if (process.argv[2]) {
    const repo = await github.api(`/repos/${repository(process.argv[2])}`);
    console.log(`Readable repository: ${repo.full_name}`);
  }
  console.log('No repository or PR was created. Fork and push permissions are checked per task before model execution.');
} catch (error) { console.error(scrub(error.message)); process.exitCode = 1; }
