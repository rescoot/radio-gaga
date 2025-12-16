#!/bin/bash
# Generates a structured changelog from conventional commits
# Usage: ./create-changelog.sh [previous_tag] [current_tag]
# If tags are not provided, they will be auto-detected

set -e

# Get tags from arguments or auto-detect
if [ -n "$1" ] && [ -n "$2" ]; then
  PREVIOUS_TAG="$1"
  CURRENT_TAG="$2"
else
  # Get the previous tag
  PREVIOUS_TAG=$(git describe --abbrev=0 --tags $(git rev-list --tags --skip=1 --max-count=1) 2>/dev/null || echo "")
  CURRENT_TAG=$(git describe --always --tags)
fi

if [ -z "$PREVIOUS_TAG" ]; then
  # First release, get all commits
  COMMIT_RANGE="$CURRENT_TAG"
else
  COMMIT_RANGE="$PREVIOUS_TAG..$CURRENT_TAG"
fi

echo "Generating changelog for range: $COMMIT_RANGE" >&2

# Generate changelog grouped by type
CHANGELOG="## What's Changed\n\n"

# Features
FEATURES=$(git log $COMMIT_RANGE --pretty=format:"%s" --no-merges | grep "^feat" || true)
if [ -n "$FEATURES" ]; then
  CHANGELOG="${CHANGELOG}### ✨ Features\n\n"
  while IFS= read -r line; do
    # Remove "feat:" or "feat(scope):" prefix
    MSG=$(echo "$line" | sed -E 's/^feat(\([^)]+\))?:\s*//')
    CHANGELOG="${CHANGELOG}- ${MSG}\n"
  done <<< "$FEATURES"
  CHANGELOG="${CHANGELOG}\n"
fi

# Fixes
FIXES=$(git log $COMMIT_RANGE --pretty=format:"%s" --no-merges | grep "^fix" || true)
if [ -n "$FIXES" ]; then
  CHANGELOG="${CHANGELOG}### 🐛 Bug Fixes\n\n"
  while IFS= read -r line; do
    MSG=$(echo "$line" | sed -E 's/^fix(\([^)]+\))?:\s*//')
    CHANGELOG="${CHANGELOG}- ${MSG}\n"
  done <<< "$FIXES"
  CHANGELOG="${CHANGELOG}\n"
fi

# Refactoring
REFACTORS=$(git log $COMMIT_RANGE --pretty=format:"%s" --no-merges | grep "^refactor" || true)
if [ -n "$REFACTORS" ]; then
  CHANGELOG="${CHANGELOG}### 🔨 Refactoring\n\n"
  while IFS= read -r line; do
    MSG=$(echo "$line" | sed -E 's/^refactor(\([^)]+\))?:\s*//')
    CHANGELOG="${CHANGELOG}- ${MSG}\n"
  done <<< "$REFACTORS"
  CHANGELOG="${CHANGELOG}\n"
fi

# Documentation
DOCS=$(git log $COMMIT_RANGE --pretty=format:"%s" --no-merges | grep "^docs" || true)
if [ -n "$DOCS" ]; then
  CHANGELOG="${CHANGELOG}### 📚 Documentation\n\n"
  while IFS= read -r line; do
    MSG=$(echo "$line" | sed -E 's/^docs(\([^)]+\))?:\s*//')
    CHANGELOG="${CHANGELOG}- ${MSG}\n"
  done <<< "$DOCS"
  CHANGELOG="${CHANGELOG}\n"
fi

# Other changes (perf, test, chore, build, ci, etc.)
OTHERS=$(git log $COMMIT_RANGE --pretty=format:"%s" --no-merges | grep -v "^feat" | grep -v "^fix" | grep -v "^refactor" | grep -v "^docs" || true)
if [ -n "$OTHERS" ]; then
  CHANGELOG="${CHANGELOG}### 🔧 Other Changes\n\n"
  while IFS= read -r line; do
    MSG=$(echo "$line" | sed -E 's/^[a-z]+(\([^)]+\))?:\s*//')
    CHANGELOG="${CHANGELOG}- ${MSG}\n"
  done <<< "$OTHERS"
  CHANGELOG="${CHANGELOG}\n"
fi

# Add full changelog link
if [ -n "$PREVIOUS_TAG" ]; then
  REPO_URL=$(git config --get remote.origin.url | sed 's/\.git$//' | sed 's|git@github.com:|https://github.com/|')
  CHANGELOG="${CHANGELOG}\n**Full Changelog**: ${REPO_URL}/compare/${PREVIOUS_TAG}...${CURRENT_TAG}"
fi

# Output the changelog
echo -e "$CHANGELOG"
