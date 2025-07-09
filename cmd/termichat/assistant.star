"""termichat assistant"""

load("json", json_decode="decode")
load("time", "now")

# Define inbuilt tools for the assistant
inbuilt_tools = [
    {
        "type": "function",
        "function": {
            "name": "save_memory",
            "description": "Save an important piece of information to memory for future conversations. Use this when the user shares preferences, instructions, or important context that should be remembered.",
            "parameters": {
                "type": "object",
                "properties": {
                    "memory": {
                        "type": "string",
                        "description": "The important information to save to memory"
                    }
                },
                "required": ["memory"]
            }
        }
    }
]

def store_new_memory(ctx, user_id, new_memory):
    """
    Store a new memory using ragstore.

    Args:
        ctx: The context of the assistant run
        user_id: The user ID for context
        new_memory: The memory to store

    Returns:
        A string indicating that the memory was stored successfully
    """
    now_timestamp = now(ctx=ctx)
    memory_id = "memory-" + str(hash(new_memory + now_timestamp))
    
    # Store the memory using ragstore
    ragstore.upsert_document(ctx=ctx, req={
        "id": memory_id,
        "kind": "memory/" + user_id,
        "attributes": {
            "user_id": user_id
        },
        "body": new_memory,
        "timestamp": now_timestamp,
        "content_type": "text/plain",
        "source": "termichat"
    })

    return "Memory stored successfully. "

def get_relevant_memories(ctx, user_id, user_input):
    """
    Get 5 latest + 5 most relevant memories for the conversation.
    
    Args:
        ctx: The context of the assistant run
        user_id: The user ID
        user_input: The current user input for relevance matching
    Returns:
        A list of memory strings
    """
    memories_dict = {}

    # Get latest memories
    latest_memories = ragstore.get_documents(ctx=ctx, req={
        "kind_prefix": "memory/" + user_id,
        "limit": 50
    })

    if latest_memories:
        for memory_doc in latest_memories:
            memory_id = memory_doc.get("id")
            if memory_id:
                memories_dict[memory_id] = memory_doc["body"]

    # Get most relevant memories via semantic search
    if user_input:
        relevant_results = ragstore.semantic_search(ctx=ctx, req={
            "semantic_query": user_input,
            "kind_prefix": "memory/" + user_id,
            "limit": 5
        })

        if relevant_results:
            for result in relevant_results:
                memory_id = result.get("id")
                memory_body = " ".join(result["chunks"])
                if memory_id:
                    memories_dict[memory_id] = memory_body

    memories = list(memories_dict.values())
    return memories

def get_conversation_history(ctx, conversation_id, count=5):
    """
    Get recent conversation history using ragstore.
    
    Args:
        ctx: The context of the assistant run
        conversation_id: The conversation ID
        count: Number of recent messages to retrieve
        
    Returns:
        A list of conversation messages with role and message content
    """
    history_docs = ragstore.get_documents(ctx=ctx, req={
        "kind_prefix": "conversation",
        "filters": {"conversation_id": conversation_id},
        "limit": count
    })
    
    messages = []
    if history_docs:
        for doc in reversed(history_docs):
            messages.append({
                "role": doc["attributes"].get("role", "user"),
                "message": doc["body"]
            })
    
    return messages

def execute_tool_calls(ctx, user_id, tool_calls):
    """
    Execute tool calls and return tool messages.

    Args:
        ctx: The context of the assistant run
        user_id: The user ID
        tool_calls: The tool calls to execute

    Returns:
        A list of tool messages
    """
    tool_messages = []
    if not tool_calls:
        return tool_messages
        
    for tool_call in tool_calls:
        function_name = tool_call["function"]["name"]
        args = json_decode(tool_call["function"]["arguments"])
        
        if function_name == "save_memory":
            # Handle inbuilt save_memory tool
            memory_content = args.get("memory", "")
            if memory_content:
                result = store_new_memory(ctx, user_id, memory_content)
                content = result
            else:
                content = "Error: No memory content provided"
        elif "_" in function_name:
            # Handle MCP tools (format: servername_toolname)
            parts = function_name.split("_", 1)
            server_name = parts[0]
            tool_name = parts[1]
            
            call_result = mcp.call_tool(ctx=ctx, req={
                "server": server_name,
                "tool": tool_name,
                "arguments": args
            })
            
            if call_result.get("isError"):
                content = "Error: " + str(call_result.get("content", "Unknown error"))
            else:
                content = str(call_result.get("content", ""))
        else:
            content = "Unknown tool: " + function_name
        
        tool_message = {
            "role": "tool",
            "tool_call_id": tool_call["id"],
            "content": content
        }
        tool_messages.append(tool_message)
    
    return tool_messages

def filter_relevant_tools(ctx, all_tools, input_text, max_tools=8):
    """
    Filter tools based on relevance to input message using LLM-based selection.
    
    Args:
        ctx: The context of the assistant run
        all_tools: List of all available tools
        input_text: The user's input message
        max_tools: Maximum number of tools to return (default: 8)
    
    Returns:
        List of the most relevant tools, up to max_tools
    """
    if not input_text or not all_tools:
        return all_tools[:max_tools]
    
    # If we have fewer tools than max_tools, return all
    if len(all_tools) <= max_tools:
        return all_tools
    
    # Prepare tool descriptions for the LLM
    tool_list = []
    for i in range(len(all_tools)):
        tool = all_tools[i]
        tool_name = tool.get("function", {}).get("name", "")
        tool_desc = tool.get("function", {}).get("description", "")
        tool_entry = str(i + 1) + ". " + tool_name + ": " + tool_desc
        tool_list.append(tool_entry)
    
    tools_text = "\n".join(tool_list)
    
    # Create prompt for LLM to select relevant tools
    system_prompt = '''You are a tool selection assistant. Given a user's input message and a list of available tools, select the {max_tools} most relevant tools that could help answer or fulfill the user's request.

    User's input: "{input_text}"

    Available tools:
    {tools_text}

    Please respond with ONLY the numbers of the {max_tools} most relevant tools, separated by commas (e.g., "1,3,5,7,9,12,15,18"). Consider:
    1. Direct relevance to the user's request
    2. Potential usefulness in completing the task
    3. Complementary tools that work well together

    If there are fewer than {max_tools} relevant tools, select all relevant ones.'''.format(
        max_tools=max_tools,
        input_text=input_text,
        tools_text=tools_text
    )

    # Make LLM call for tool selection
    result = openai.complete(ctx=ctx, req={
        "messages": [{"role": "system", "content": system_prompt}],
        "model": "gpt-4o-mini",
        "temperature": 0.1,
        "max_tokens": 100
    })
    
    if not result.get("choices", []):
        # Fallback: return first max_tools if LLM call fails
        return all_tools[:max_tools]
    
    # Parse the response to get selected tool indices
    response_content = result["choices"][0]["message"]["content"].strip()
    selected_indices = []
    
    # Extract numbers from the response
    parts = response_content.split(",")
    for part in parts:
        part = part.strip()
        # Simple integer parsing since Starlark doesn't have try/except
        if part.isdigit():
            index = int(part) - 1  # Convert to 0-based indexing
            if 0 <= index and index < len(all_tools):
                selected_indices.append(index)
    
    # Return selected tools
    if selected_indices:
        selected_tools = []
        for i in selected_indices[:max_tools]:
            selected_tools.append(all_tools[i])
        return selected_tools
    else:
        # Fallback: return first max_tools if parsing fails
        return all_tools[:max_tools]

def main(ctx, input):
    """
    Main function for the assistant.

    Args:
        ctx: The context of the assistant run
        input: A dictionary containing the conversation ID, user ID, and message

    Returns:
        A dictionary containing the response from the assistant
    """

    # Use default conversation ID since input is just a string
    conversation_id = input.get("conversation_id", "default")
    user_id = input.get("user_id", "default")
    input_text = input.get("message", "")

    # Load conversation history, memories and MCP tools available to the assistant
    old_messages = get_conversation_history(ctx, conversation_id, count=5)
    memories = get_relevant_memories(ctx, user_id, input_text)
    all_tools = mcp.list_tools(ctx=ctx, req={}) + inbuilt_tools
    
    # Filter tools to get the 8 most relevant ones based on input message
    relevant_tools = filter_relevant_tools(ctx, all_tools, input_text, max_tools=8)
    
    system_prompt = "You are a helpful assistant.\n\n"
    if memories:
        system_prompt += '''
        Recent memories from our past conversations:
        {memories_text}
        
        IMPORTANT: 
        1. Follow any instructions contained in these memories strictly. 
        2. If a memory contains specific instructions, preferences, or requirements, you must adhere to them exactly. 
        3. Use these memories to provide more contextual and personalized assistance.
        4. If the user asks a question that is not related to the memories, you should respond with "I'm sorry, I don't know about that."
        '''.format(memories_text="\n".join(["- " + mem for mem in memories]))

    messages = [{"role": "system", "content": system_prompt}]
    for message in old_messages:
        messages.append({"role": message.get("role"), "content": message.get("message")})

    max_iterations = 5
    for iteration in range(max_iterations):
        is_final_iteration = (iteration == max_iterations - 1)

        completion_params = {
            "messages": messages,
            "model": "gpt-4.1-mini",
            "temperature": 0.5
        }

        if is_final_iteration:
            messages.append({
                "role": "system", 
                "content": '''
                This is the final iteration of the conversation. 
                Please provide a comprehensive answer and include suggestions on: 
                1. What progress has been made so far 
                2. What specific next steps the user should take 
                3. Any important considerations or potential challenges 
                4. How the user can continue or resume this work
                '''
            })
        else:
            completion_params["tools"] = relevant_tools
        
        result = openai.complete(ctx=ctx, req=completion_params)
        if not result.get("choices", []):
            return "No response from the model"
        
        assistant_message = result["choices"][0]["message"]
        messages.append(assistant_message)
        
        has_tool_calls = assistant_message.get("tool_calls")
        if not has_tool_calls:
            break

        tool_messages = execute_tool_calls(ctx, user_id, assistant_message["tool_calls"])
        if tool_messages:
            messages.extend(tool_messages)
    
    return messages[-1].get("content", "No content in the model response")