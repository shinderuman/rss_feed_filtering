#!/bin/bash

set -e

# Constants
readonly SCRIPT_NAME="$(basename "$0")"
readonly FUNCTION_NAME="rssFeedFiltering"
readonly AWS_PROFILE="lambda-deploy"

# Function to show usage
show_usage() {
    cat << EOF
Usage: $SCRIPT_NAME [--build-only]

Options:
  -b, --build-only  Only build the function, don't deploy
  -h, --help        Show this help message

Examples:
  $SCRIPT_NAME              # Build and deploy
  $SCRIPT_NAME -b           # Build only
EOF
}

# Function to build the Lambda function
build_function() {
    echo "Building $FUNCTION_NAME..."
    
    # Set environment variables for cross-compilation
    export GOOS=linux
    export GOARCH=amd64
    export CGO_ENABLED=0
    
    # Build the binary
    go build -ldflags="-s -w" -o bootstrap main.go
    
    # Create zip file
    zip -r lambda.zip bootstrap
    
    echo "✅ Built $FUNCTION_NAME successfully"
}

# Function to deploy the Lambda function
deploy_function() {
    echo "Deploying $FUNCTION_NAME..."
    
    # Get current function info before deployment
    local current_info
    current_info=$(aws lambda get-function \
        --profile "$AWS_PROFILE" \
        --function-name "$FUNCTION_NAME" \
        --output json 2>/dev/null || echo "{}")
    
    local old_code_size
    old_code_size=$(echo "$current_info" | jq -r '.Configuration.CodeSize // 0')
    
    # Update Lambda function code
    local response
    response=$(aws lambda update-function-code \
        --profile "$AWS_PROFILE" \
        --function-name "$FUNCTION_NAME" \
        --zip-file fileb://lambda.zip \
        --output json 2>&1)
    
    local exit_code=$?
    
    if [ $exit_code -eq 0 ]; then
        # Parse deployment information
        local code_size last_modified runtime version code_sha256 memory_size timeout
        
        code_size=$(echo "$response" | jq -r '.CodeSize // "N/A"')
        last_modified=$(echo "$response" | jq -r '.LastModified // "N/A"')
        runtime=$(echo "$response" | jq -r '.Runtime // "N/A"')
        version=$(echo "$response" | jq -r '.Version // "N/A"')
        code_sha256=$(echo "$response" | jq -r '.CodeSha256 // "N/A"')
        memory_size=$(echo "$response" | jq -r '.MemorySize // "N/A"')
        timeout=$(echo "$response" | jq -r '.Timeout // "N/A"')
        
        # Calculate size difference
        local size_diff=""
        if [ "$old_code_size" != "0" ] && [ "$code_size" != "N/A" ]; then
            local diff=$((code_size - old_code_size))
            if [ $diff -gt 0 ]; then
                size_diff=" (+$(format_bytes $diff))"
            elif [ $diff -lt 0 ]; then
                size_diff=" ($(format_bytes $diff))"
            else
                size_diff=" (no change)"
            fi
        fi
        
        echo "✅ Deployed $FUNCTION_NAME successfully"
        echo "   📦 Code Size: $(format_bytes "$code_size")$size_diff"
        echo "   🕒 Last Modified: $(format_timestamp "$last_modified")"
        echo "   🔧 Runtime: $runtime"
        echo "   💾 Memory: ${memory_size} MB"
        echo "   ⏱️  Timeout: $(format_timeout "$timeout")"
        echo "   🔑 SHA256: ${code_sha256:0:12}..."
        echo "   📋 Version: $version"
        
        # Get function URL if exists
        local function_url
        function_url=$(aws lambda get-function-url-config \
            --profile "$AWS_PROFILE" \
            --function-name "$FUNCTION_NAME" \
            --output json 2>/dev/null | jq -r '.FunctionUrl // empty')
        
        if [ -n "$function_url" ]; then
            # Get token info from main.go
            local token_info
            token_info=$(get_token_info)
            
            echo "   🌐 Function URL: $function_url"
            echo "   📡 RSS Feed URL: ${function_url}?category=CATEGORY&${token_info}"
            echo "      Example: ${function_url}?category=tech&${token_info}"
        fi
        
        # Get API Gateway URL if exists
        local api_gateway_url
        local region
        region=$(aws configure get region --profile "$AWS_PROFILE" 2>/dev/null)
        
        # Try to find API Gateway v2 (HTTP API) first, then v1 (REST API)
        local api_gateway_url=""
        local api_name=""
        
        # Check API Gateway v2 (HTTP API)
        local v2_apis
        v2_apis=$(aws apigatewayv2 get-apis \
            --profile "$AWS_PROFILE" \
            --region "$region" \
            --output json 2>/dev/null | jq -r '.Items[]? | select(.Name | contains("'$FUNCTION_NAME'")) | .ApiEndpoint + "|" + .Name' 2>/dev/null)
        
        if [ -n "$v2_apis" ]; then
            echo "$v2_apis" | head -1 | while IFS='|' read -r endpoint name; do
                if [ -n "$endpoint" ]; then
                    # Extract API ID from endpoint
                    local api_id
                    api_id=$(echo "$endpoint" | sed 's|https://||' | cut -d'.' -f1)
                    
                    # Get stage and route information
                    local stage_name
                    stage_name=$(aws apigatewayv2 get-stages \
                        --api-id "$api_id" \
                        --profile "$AWS_PROFILE" \
                        --region "$region" \
                        --output json 2>/dev/null | jq -r '.Items[0].StageName // "default"' 2>/dev/null)
                    
                    local route_path
                    route_path=$(aws apigatewayv2 get-routes \
                        --api-id "$api_id" \
                        --profile "$AWS_PROFILE" \
                        --region "$region" \
                        --output json 2>/dev/null | jq -r '.Items[0].RouteKey // ""' 2>/dev/null | sed 's/ANY //')
                    
                    # Construct full URL
                    local full_url="${endpoint}/${stage_name}${route_path}"
                    
                    echo "   🔗 API Gateway v2: $full_url ($name)"
                    
                    # Get token info from main.go
                    local token_info
                    token_info=$(get_token_info)
                    
                    echo "   📡 RSS Feed URL: ${full_url}?${token_info}&category=CATEGORY"
                    echo "      Example: ${full_url}?${token_info}&category=comic_series"
                fi
            done
        else
            # Fallback if no API Gateway v2 found
            echo "   ⚠️  API Gateway v2: Could not find matching API (check AWS console)"
            
            # Get token info from main.go for fallback case
            local token_info
            token_info=$(get_token_info)
            
            echo "   📡 RSS Feed URL: https://YOUR-API-ID.execute-api.${region}.amazonaws.com/?category=CATEGORY&${token_info}"
        fi
        
    else
        echo "❌ Failed to deploy $FUNCTION_NAME"
        echo "Error details:"
        echo "$response"
        return 1
    fi
}

# Function to format bytes into human readable format
format_bytes() {
    local bytes="$1"
    
    if [ "$bytes" = "N/A" ] || [ -z "$bytes" ]; then
        echo "N/A"
        return
    fi
    
    # Handle negative numbers for size differences
    local sign=""
    if [ "$bytes" -lt 0 ]; then
        sign="-"
        bytes=$((bytes * -1))
    fi
    
    if [ "$bytes" -lt 1024 ]; then
        echo "${sign}${bytes} B"
    elif [ "$bytes" -lt 1048576 ]; then
        echo "${sign}$((bytes / 1024)) KB"
    elif [ "$bytes" -lt 1073741824 ]; then
        echo "${sign}$((bytes / 1048576)) MB"
    else
        echo "${sign}$((bytes / 1073741824)) GB"
    fi
}

# Function to format timestamp into readable format (UTC)
format_timestamp() {
    local timestamp="$1"
    
    if [ "$timestamp" = "N/A" ] || [ -z "$timestamp" ]; then
        echo "N/A"
        return
    fi
    
    # Convert ISO 8601 timestamp to readable UTC format
    local clean_timestamp="${timestamp%.*}"  # Remove milliseconds
    clean_timestamp="${clean_timestamp%+*}"  # Remove timezone info
    clean_timestamp="${clean_timestamp}Z"    # Add UTC indicator
    
    # Simple format conversion: 2025-08-10T16:14:58Z -> 2025-08-10 16:14:58 UTC
    echo "${clean_timestamp}" | sed 's/T/ /' | sed 's/Z/ UTC/'
}

# Function to extract token information from main.go
get_token_info() {
    local token_key
    local token_value
    
    token_key=$(grep -o 'accessTokenKey = "[^"]*"' main.go | sed 's/accessTokenKey = "//;s/"//' 2>/dev/null || echo "token")
    token_value=$(grep -o 'accessTokenVal = "[^"]*"' main.go | sed 's/accessTokenVal = "//;s/"//' 2>/dev/null || echo "TOKEN")
    
    echo "${token_key}=${token_value}"
}

# Function to format timeout into readable format
format_timeout() {
    local seconds="$1"
    
    if [ "$seconds" = "N/A" ] || [ -z "$seconds" ]; then
        echo "N/A"
        return
    fi
    
    local minutes=$((seconds / 60))
    local remaining_seconds=$((seconds % 60))
    
    if [ $minutes -gt 0 ]; then
        if [ $remaining_seconds -gt 0 ]; then
            echo "${minutes}m ${remaining_seconds}s (${seconds}s)"
        else
            echo "${minutes}m (${seconds}s)"
        fi
    else
        echo "${seconds}s"
    fi
}

# Function to clean up temporary files
cleanup() {
    rm -f bootstrap lambda.zip
}

# Function to check if jq is installed
check_jq() {
    if ! command -v jq >/dev/null 2>&1; then
        echo "Error: jq is required but not installed"
        echo "Please install jq to parse JSON responses from AWS CLI"
        echo ""
        echo "Installation instructions:"
        echo "  macOS: brew install jq"
        exit 1
    fi
}

# Function to validate AWS profile
validate_aws_profile() {
    if ! aws configure list-profiles | grep -q "^$AWS_PROFILE$"; then
        echo "Error: AWS profile '$AWS_PROFILE' not found"
        echo "Please configure the profile or update AWS_PROFILE in .env file"
        exit 1
    fi
}

# Function to parse command line arguments
parse_arguments() {
    BUILD_ONLY=false

    while [[ $# -gt 0 ]]; do
        case $1 in
            -b|--build-only)
                BUILD_ONLY=true
                shift
                ;;
            -h|--help)
                show_usage
                exit 0
                ;;
            *)
                echo "Unknown option: $1"
                show_usage
                exit 1
                ;;
        esac
    done
}

# Main function
main() {
    # Parse command line arguments
    parse_arguments "$@"
    
    # Check required dependencies
    check_jq
    
    # Validate AWS profile
    validate_aws_profile
    
    # Display configuration
    echo "Using AWS profile: $AWS_PROFILE"
    echo "Function name: $FUNCTION_NAME"
    if [ "$BUILD_ONLY" = true ]; then
        echo "Mode: Build only"
    else
        echo "Mode: Build and deploy"
    fi
    echo ""
    
    # Build the function
    build_function
    
    # Deploy if not build-only
    if [ "$BUILD_ONLY" != true ]; then
        deploy_function
    fi
    
    # Clean up
    cleanup
    
    echo ""
    echo "🎉 Operation completed successfully!"
    
    # Display summary information
    if [ "$BUILD_ONLY" != true ]; then
        echo ""
        echo "📊 Deployment Summary:"
        echo "   🔗 AWS Console: https://console.aws.amazon.com/lambda/home?region=$(aws configure get region --profile "$AWS_PROFILE" 2>/dev/null)#/functions/$FUNCTION_NAME"
        echo "   📈 CloudWatch Logs: https://console.aws.amazon.com/cloudwatch/home?region=$(aws configure get region --profile "$AWS_PROFILE" 2>/dev/null)#logsV2:log-groups/log-group/%252Faws%252Flambda%252F$FUNCTION_NAME"
    fi
}

# Execute main function with all arguments
main "$@"