// Package winjob contains a child process tree in a Windows Job Object
// (Story 78.34): the child starts suspended, is assigned to its own job with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE and no breakaway, and only then
// resumes, so no descendant can start outside the job. Closing the job
// handle (explicitly, or because the owning process exited or crashed)
// kills every process in it. The job handle is never inheritable.
//
// archivist connect uses it for each harness session, and the Windows
// background service supervisor for the daemon it runs. On other platforms
// the package is empty.
package winjob
