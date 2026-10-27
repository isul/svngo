/*
 * svngo-authz: minimal svnauthz-compatible authz checker built on libsvn_repos,
 * the same authz engine used by mod_authz_svn.
 *
 *   svngo-authz validate FILE
 *   svngo-authz accessof --username USER --repository REPO --path PATH FILE
 *
 * accessof prints "rw", "r" or "no" (same output as svnauthz accessof).
 */
#include <stdio.h>
#include <string.h>
#include <stdlib.h>

#include <apr_general.h>
#include <svn_cmdline.h>
#include <svn_dirent_uri.h>
#include <svn_error.h>
#include <svn_pools.h>
#include <svn_repos.h>
#include <svn_utf.h>

static int usage(void)
{
  fputs("usage: svngo-authz validate FILE\n"
        "       svngo-authz accessof --username USER --repository REPO --path PATH FILE\n",
        stderr);
  return 2;
}

static int fail(svn_error_t *err)
{
  svn_handle_error2(err, stderr, FALSE, "svngo-authz: ");
  svn_error_clear(err);
  return 1;
}

static void warn(void *baton, const svn_error_t *error, apr_pool_t *pool)
{
  (void)baton;
  (void)pool;
  if (error && error->message)
    fprintf(stderr, "svngo-authz: warning: %s\n", error->message);
}

static svn_error_t *read_authz(svn_authz_t **authz, const char *file, apr_pool_t *pool)
{
  const char *utf8, *abs;
  SVN_ERR(svn_utf_cstring_to_utf8(&utf8, file, pool));
  SVN_ERR(svn_dirent_get_absolute(&abs, svn_dirent_internal_style(utf8, pool), pool));
  return svn_repos_authz_read4(authz, abs, NULL, TRUE, NULL, warn, NULL, pool, pool);
}

int main(int argc, const char **argv)
{
  apr_pool_t *pool;
  svn_authz_t *authz;
  svn_error_t *err;

  if (svn_cmdline_init("svngo-authz", stderr) != EXIT_SUCCESS)
    return 2;
  pool = svn_pool_create(NULL);

  if (argc == 3 && strcmp(argv[1], "validate") == 0)
    {
      err = read_authz(&authz, argv[2], pool);
      return err ? fail(err) : 0;
    }

  if (argc >= 3 && strcmp(argv[1], "accessof") == 0)
    {
      const char *user = NULL, *repo = NULL, *path = NULL, *file = NULL;
      svn_boolean_t can_read = FALSE, can_write = FALSE;
      int i;

      for (i = 2; i < argc; i++)
        {
          if (strcmp(argv[i], "--username") == 0 && i + 1 < argc)
            user = argv[++i];
          else if (strcmp(argv[i], "--repository") == 0 && i + 1 < argc)
            repo = argv[++i];
          else if (strcmp(argv[i], "--path") == 0 && i + 1 < argc)
            path = argv[++i];
          else if (argv[i][0] != '-' && file == NULL)
            file = argv[i];
          else
            return usage();
        }
      if (!file || !repo || !path)
        return usage();

      err = read_authz(&authz, file, pool);
      if (!err && user)
        err = svn_utf_cstring_to_utf8(&user, user, pool);
      if (!err)
        err = svn_utf_cstring_to_utf8(&repo, repo, pool);
      if (!err)
        err = svn_utf_cstring_to_utf8(&path, path, pool);
      if (!err)
        err = svn_repos_authz_check_access(authz, repo, path, user, svn_authz_read, &can_read, pool);
      if (!err)
        err = svn_repos_authz_check_access(authz, repo, path, user, svn_authz_write, &can_write, pool);
      if (err)
        return fail(err);
      puts(can_write ? "rw" : (can_read ? "r" : "no"));
      return 0;
    }

  return usage();
}
