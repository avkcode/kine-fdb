// Jenkins pipeline for kine-fdb. It uses only the scripted Pipeline steps
// node, stage, and sh, and clones the source with git, so it needs neither
// the Git SCM plugin nor Declarative Pipeline. Each stage is a step of
// hack/ci/run-ci.sh, which runs everything in Docker; the agent only needs a
// Docker daemon.
//
// Optional job parameters / environment variables:
//   KINE_FDB_REPO    repository to clone (default: the GitHub repository)
//   KINE_FDB_BRANCH  branch or tag to build (default: devel)
//   SOAK_SECONDS     duration of the K3s soak test (default: 600; 0 skips it)

def repo = env.KINE_FDB_REPO ?: 'https://github.com/avkcode/kine-fdb.git'
def branch = env.KINE_FDB_BRANCH ?: 'devel'

node {
    try {
        stage('Checkout') {
            sh """
                rm -rf src
                git clone --depth 1 --branch '${branch}' '${repo}' src
                git -C src log -1 --oneline
            """
        }
        for (s in ['env', 'lint', 'test', 'apiserver', 'image', 'tls', 'soak']) {
            stage(s) {
                sh "cd src && ./hack/ci/run-ci.sh ${s}"
            }
        }
    } finally {
        stage('cleanup') {
            sh 'cd src 2>/dev/null && ./hack/ci/run-ci.sh cleanup || true'
        }
    }
}
